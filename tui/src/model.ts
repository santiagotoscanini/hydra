// What the dashboard shows, worked out from a snapshot. Pure: no I/O.

import type { Health, PR, Problem, Service, Snapshot, Update } from "./types.js";

/** One row of the list: the main checkout, a worktree, or a branch of a PR
 *  stack that no worktree has checked out (kind "branch": nothing runs it,
 *  but the worktrees stacked on it run its code). */
export interface Entry {
	kind: "main" | "worktree" | "branch";
	/** A checkout's path; for a branch with no checkout, "branch:" and its name. */
	root: string;
	name: string;
	branch: string;
	slot: number;
	up: boolean;
	commit: string;
	subject: string;
	/** Commits behind main (for main: behind its upstream). */
	behind: number;
	/** The branch it is updated from: main's, or the one it is stacked on. */
	parent?: string;
	/** Commits that parent has that it doesn't. */
	behindParent: number;
	ahead: number;
	dirty: number;
	stack: string[];
	services: Service[];
	problems: Problem[];
	update?: Update;
	pr?: PR;
	outOfStep: string[];
	mainBehind: boolean;
	/** hydra stopped its stack after this long unused ("2h"). */
	idleStopped?: string;
	/** Far behind main: past behind_warn, or merging main conflicts (the files). */
	farBehind?: { behind: number; conflicts: string[] };
	memory: number;
	pending: boolean;
	error?: string;
	current: boolean;
	/** The tunnels it holds (main: those no worktree holds). */
	tunnels: string[];
}

export function entries(s: Snapshot): Entry[] {
	const out: Entry[] = [];
	if (s.main) {
		const m = s.main;
		out.push({
			kind: "main",
			root: m.root,
			name: "main",
			branch: m.branch,
			slot: 0,
			up: m.up,
			commit: m.commit,
			subject: m.subject,
			behind: m.behind,
			behindParent: 0,
			ahead: 0,
			dirty: m.dirty,
			stack: [],
			services: m.processes,
			problems: m.problems,
			outOfStep: [],
			mainBehind: false,
			memory: m.memory,
			pending: false,
			current: s.current === m.root,
			tunnels: (s.tunnels ?? []).filter((t) => !t.holder).map((t) => t.name),
		});
	}
	for (const w of s.worktrees) {
		out.push({
			kind: "worktree",
			root: w.root,
			name: w.dir,
			branch: w.branch || "(detached)",
			slot: w.slot,
			up: w.up,
			commit: w.commit,
			subject: w.subject,
			behind: w.behind,
			parent: w.update?.parent,
			// A snapshot from before update.behind: on main, the two are one.
			behindParent: w.update?.behind ?? ((w.stack?.length ?? 0) <= 1 ? w.behind : 0),
			ahead: w.ahead,
			dirty: 0,
			stack: w.stack ?? [],
			services: w.services,
			problems: w.problems,
			update: w.update,
			pr: w.pr,
			outOfStep: w.outOfStep.map((k) => k.service),
			mainBehind: !!w.mainBehind,
			idleStopped: w.idleStopped?.after,
			farBehind: w.farBehind && { behind: w.farBehind.behind, conflicts: w.farBehind.conflicts ?? [] },
			memory: w.memory,
			pending: !!w.pending,
			error: w.error,
			current: s.current === w.root,
			tunnels: w.tunnel ?? [],
		});
	}
	for (const b of s.branches ?? []) {
		out.push({
			kind: "branch",
			root: branchRoot(b.branch),
			name: b.branch,
			branch: b.branch,
			slot: 0,
			up: false,
			commit: b.commit,
			subject: b.subject,
			behind: b.behind,
			parent: b.update?.parent,
			behindParent: b.update?.behind ?? 0,
			ahead: b.ahead,
			dirty: 0,
			stack: b.stack,
			services: [],
			problems: [],
			update: b.update,
			pr: b.pr,
			outOfStep: [],
			mainBehind: false,
			memory: 0,
			pending: !!b.pending,
			current: false,
			tunnels: [],
		});
	}
	return out;
}

/** The key a branch with no checkout goes by, in the place of a path. */
export const branchRoot = (branch: string) => "branch:" + branch;

/** The services a checkout runs itself and that aren't off until started. */
export function runsHere(e: Entry): Service[] {
	return e.services.filter((s) => s.runs === "here" && !s.off);
}

/** A branch as a person names it: without the "user/" prefix, and split
 *  into its ticket id and title words when it starts with one
 *  ("santiago/fla-709-an-advisor" → fla-709, "an advisor"). */
export function branchName(branch: string): { id?: string; title: string } {
	const slug = branch.slice(branch.lastIndexOf("/") + 1);
	const m = /^([A-Za-z]{2,10}-\d+)(?:[-_](.*))?$/.exec(slug);
	if (!m) return { title: slug };
	return { id: m[1]!.toLowerCase(), title: (m[2] ?? "").replace(/[-_]+/g, " ") };
}

/** The shortest name that still says which branch: its ticket id, or its slug. */
export function shortBranch(branch: string): string {
	const n = branchName(branch);
	return n.id ?? n.title;
}

/** Names for branches said together: each its shortest name, except that
 *  branches sharing a ticket id (a PR stack: scrm-264-app-ring-schema,
 *  -core, -web) are told apart by what follows the words their titles
 *  share: schema, core, web. */
export function labels(branches: string[]): Map<string, string> {
	const out = new Map<string, string>();
	const groups = new Map<string, string[]>();
	for (const b of new Set(branches)) groups.set(shortBranch(b), [...(groups.get(shortBranch(b)) ?? []), b]);
	for (const [short, group] of groups) {
		if (group.length === 1) {
			out.set(group[0]!, short);
			continue;
		}
		const words = group.map((b) => branchName(b).title.split(" "));
		let common = 0;
		while (words.every((w) => w.length > common + 1 && w[common] === words[0]![common])) common++;
		for (const [i, b] of group.entries()) out.set(b, words[i]!.slice(common).join(" ") || branchName(b).title || short);
	}
	return out;
}

/** branch's name said next to other's: "schema" beside scrm-264-app-ring-web. */
export function labelBeside(branch: string, other: string): string {
	return labels([branch, other]).get(branch)!;
}

/** What a checkout is called in the list and the panel. */
export function displayName(e: Entry): { id?: string; title: string } {
	if (e.kind === "main") return { title: "main" };
	if (e.branch === "(detached)") return { title: e.name };
	return branchName(e.branch);
}

/** The stack's progress: how many of what it runs itself are ready. */
export function progress(e: Entry): { ready: number; total: number } {
	const svcs = e.kind === "main" ? e.services.filter((s) => s.health !== "off") : runsHere(e);
	return { ready: svcs.filter((s) => s.health === "up" || s.health === "done").length, total: svcs.length };
}

export interface StateView {
	glyph: string;
	/** a spinner instead of the glyph: something is under way */
	spin: boolean;
	word: string;
	color: string;
}

/** Where a checkout's stack stands, in a glyph and a word or two. busy is
 *  the action the dashboard is running in it ("starting", "stopping"). */
export function stateOf(e: Entry, busy?: string): StateView {
	if (e.kind === "branch") {
		if (busy) return { glyph: "", spin: true, word: `${busy}…`, color: "yellow" };
		return { glyph: "⎇", spin: false, word: "branch only", color: "gray" };
	}
	if (e.error) return { glyph: "✗", spin: false, word: "can't plan", color: "red" };
	if (e.pending) return { glyph: "…", spin: true, word: "planning", color: "gray" };
	const p = progress(e);
	const svcs = e.kind === "main" ? e.services : runsHere(e);
	const starting = e.up && svcs.some((s) => s.health === "starting");
	if (busy) {
		const word = busy === "starting" && starting ? `starting ${p.ready}/${p.total}` : `${busy}…`;
		return { glyph: "", spin: true, word, color: "yellow" };
	}
	if (!e.up) {
		if (e.idleStopped) return { glyph: "◌", spin: false, word: "idle-stopped", color: "blue" };
		return { glyph: "○", spin: false, word: "down", color: e.kind === "main" ? "red" : "gray" };
	}
	if (svcs.some((s) => s.health === "failed")) return { glyph: "✗", spin: false, word: "failing", color: "red" };
	if (starting) return { glyph: "", spin: true, word: `starting ${p.ready}/${p.total}`, color: "yellow" };
	const blocking = e.problems.filter((x) => x.blocks).length;
	if (blocking) return { glyph: "●", spin: false, word: `up, ${blocking} issue${blocking === 1 ? "" : "s"}`, color: "yellow" };
	return { glyph: "●", spin: false, word: "up", color: "green" };
}

/** How many files merging main would conflict in. */
export function conflictCount(e: Entry): number {
	return e.update?.conflicts?.length || e.farBehind?.conflicts.length || 0;
}

/** How many files merging its parent in would conflict in. farBehind's
 *  files are main's: they count only for a branch that sits on main. */
export function parentConflicts(e: Entry): number {
	return e.update?.conflicts?.length || (onMain(e) ? (e.farBehind?.conflicts.length ?? 0) : 0);
}

const onMain = (e: Entry) => e.stack.length <= 1;

/** Everything on it is already in its parent: it was merged upstream. */
export function landed(e: Entry): boolean {
	return !!e.update?.note.startsWith("left alone: everything on it");
}

/** Behind the branch it is updated from, so what runs there isn't what its
 *  parent has. */
function ownStale(e: Entry): boolean {
	return e.kind !== "main" && !e.error && !e.pending && !landed(e) && (e.behindParent > 0 || parentConflicts(e) > 0);
}

/**
 * Where a checkout stands against its parent, the staleness of the chain
 * above it carried down. hydra runs a branch on the code of every branch
 * under it, so a branch in step with a parent that is behind is behind too.
 */
export type Staleness =
	/** in step with its parent, and so is every branch up to main */
	| { kind: "current" }
	/** behind its parent; from: the branch up the chain that is behind too,
	 *  none when the chain goes stale here */
	| { kind: "behind"; behind: number; conflicts: number; from?: Entry }
	/** in step with its parent, but from, up the chain, is behind its own */
	| { kind: "inherits"; from: Entry }
	| { kind: "landed" }
	/** main, or a worktree not worked out yet */
	| { kind: "none" };

/** A checkout's place in the tree: main the root, each worktree under its parent. */
export interface Node {
	e: Entry;
	depth: number;
	/** the connectors drawn before its glyph ("│ ├─"), two columns a level */
	lead: string;
	/** the checkouts above it, main first */
	chain: Entry[];
	stale: Staleness;
}

/** What a sibling is sorted by: its name as shown, which doesn't change
 *  when its stack starts or stops, so rows stay where they are. */
function sortKey(e: Entry): string {
	const n = displayName(e);
	return (n.id ?? n.title).toLowerCase() + " " + n.title.toLowerCase();
}

/**
 * The checkouts as a tree, in the order drawn: main, then each branch
 * right above the branches stacked on it, siblings by name. A worktree's
 * parent is the worktree, or the stack branch with no checkout, whose
 * branch it is updated from; the rest (main, or not worked out yet) hang
 * off main.
 */
export function tree(list: Entry[]): Node[] {
	const main = list.find((e) => e.kind === "main");
	const byBranch = new Map(list.filter((e) => e.kind !== "main").map((e) => [e.branch, e]));
	const parentOf = new Map<Entry, Entry | undefined>();
	for (const e of list) {
		if (e.kind === "main") continue;
		const p = e.parent ? byBranch.get(e.parent) : undefined;
		parentOf.set(e, p && p !== e ? p : main);
	}
	// A loop (two worktrees each naming the other) can't be drawn: its
	// branches hang off main.
	for (const e of parentOf.keys()) {
		const seen = new Set<Entry>([e]);
		for (let p = parentOf.get(e); p && p !== main; p = parentOf.get(p)) {
			if (seen.has(p)) {
				parentOf.set(e, main);
				break;
			}
			seen.add(p);
		}
	}
	const children = new Map<Entry | undefined, Entry[]>();
	for (const [e, p] of parentOf) children.set(p, [...(children.get(p) ?? []), e]);
	for (const kids of children.values()) kids.sort((a, b) => sortKey(a).localeCompare(sortKey(b)));

	const out: Node[] = [];
	const visit = (e: Entry, depth: number, lead: string, below: string, chain: Entry[], firstStale?: Entry) => {
		let stale: Staleness;
		if (e.kind === "main" || e.error || e.pending) stale = { kind: "none" };
		else if (landed(e)) stale = { kind: "landed" };
		else if (ownStale(e)) stale = { kind: "behind", behind: e.behindParent, conflicts: parentConflicts(e), from: firstStale };
		else if (firstStale) stale = { kind: "inherits", from: firstStale };
		else stale = { kind: "current" };
		out.push({ e, depth, lead, chain, stale });
		const kids = children.get(e) ?? [];
		kids.forEach((k, i) => {
			const last = i === kids.length - 1;
			visit(k, depth + 1, below + (last ? "└─" : "├─"), below + (last ? "  " : "│ "), [...chain, e], firstStale ?? (ownStale(e) ? e : undefined));
		});
	};
	if (main) visit(main, 0, "", "", []);
	// Without main (it couldn't be read), the branches on it are roots.
	else for (const e of children.get(undefined) ?? []) visit(e, 0, "", "", []);
	return out;
}

/** Out of date: behind its parent, or on a branch that is. */
export function outOfDate(n: Node): boolean {
	return n.stale.kind === "behind" || n.stale.kind === "inherits";
}

export const plural = (n: number, one: string, many = one + "s") => `${n} ${n === 1 ? one : many}`;

/** How a checkout stands against its parent, for the list. texts are the
 *  ways to say it, longest first: the row shows the first that fits. */
export interface Badge {
	texts: string[];
	color?: string;
	bold?: boolean;
	dim?: boolean;
	/** a mark before the text, in the colour of staleness: "↑" (inherited) */
	mark?: string;
}

export function badgeOf(n: Node): Badge {
	const e = n.e;
	// Its PR is merged or closed: where it stands doesn't matter any more.
	if (prDone(e.pr)) return { texts: [`PR ${e.pr!.state}: nothing to do`, `PR ${e.pr!.state}`], color: "magenta", bold: true };
	if (e.kind === "main") {
		const parts: string[] = [];
		if (e.behind > 0) parts.push(`${e.behind} behind upstream`);
		if (e.dirty > 0) parts.push(`${e.dirty} uncommitted`);
		return parts.length ? { texts: [parts.join(" · ")], color: "yellow" } : { texts: ["up to date"], dim: true };
	}
	const s = n.stale;
	const parent = e.parent ? labelBeside(e.parent, e.branch) : "main";
	switch (s.kind) {
		case "none":
			return { texts: [""] };
		case "landed":
			return { texts: [`landed in ${parent}`, "landed"], dim: true };
		case "behind": {
			const c = s.conflicts ? ` · ${plural(s.conflicts, "conflict")}` : "";
			const b = s.behind ? `${s.behind} behind` : "";
			const texts = b ? [`${b} ${parent}${c}`, b + c] : [plural(s.conflicts, "conflict")];
			// Where the chain goes stale is drawn strong; below that, plain.
			return { texts, color: s.conflicts ? "red" : "yellow", bold: !s.from };
		}
		case "inherits": {
			const from = labelBeside(s.from.branch, e.branch);
			return { texts: [`${from} is behind`, `${from} behind`, "parent behind"], dim: true, mark: "↑ " };
		}
		case "current":
			if (e.outOfStep.length > 0 && e.mainBehind) return { texts: ["in step · main is older", "main is older"], color: "yellow" };
			return { texts: ["in step"], dim: true };
	}
}

/** The branch to update first for a checkout to be current, and how. */
export function firstToUpdate(n: Node): Entry | undefined {
	if (n.stale.kind === "inherits") return n.stale.from;
	if (n.stale.kind === "behind") return n.stale.from ?? n.e;
	return undefined;
}

export function healthGlyph(h: Health): { glyph: string; color?: string } {
	switch (h) {
		case "up":
			return { glyph: "●", color: "green" };
		case "done":
			return { glyph: "✓", color: "gray" };
		case "starting":
			return { glyph: "◐", color: "yellow" };
		case "failed":
			return { glyph: "✗", color: "red" };
		case "off":
			return { glyph: "○", color: "gray" };
		default:
			return { glyph: "?", color: "gray" };
	}
}

/** The services worth showing in the detail: everything this checkout runs,
 *  then main's copies it uses. hidden counts main's that nothing here uses. */
export function detailServices(e: Entry): { shown: Service[]; hidden: number } {
	if (e.kind === "main") return { shown: e.services, hidden: 0 };
	const here = e.services.filter((s) => s.runs === "here");
	const fromMain = e.services.filter((s) => s.runs !== "here" && (s.used || s.runs === "routed"));
	return { shown: [...here, ...fromMain], hidden: e.services.length - here.length - fromMain.length };
}

export function bytes(n?: number): string {
	if (!n) return "";
	if (n >= 1 << 30) return `${(n / (1 << 30)).toFixed(1)}G`;
	return `${Math.round(n / (1 << 20))}M`;
}

export function shQuote(s: string): string {
	if (/^[\w@%+=:,./-]+$/.test(s)) return s;
	return `'${s.replace(/'/g, `'\\''`)}'`;
}

/** A fix, as the shell command to run: every `hydra` in command position
 *  becomes the hydra that runs this dashboard. */
export function fixCommand(run: string, exe: string): string {
	return run.replace(/(^|&&\s*|;\s*)hydra(?=\s|$)/g, (_m, pre: string) => pre + shQuote(exe));
}

/** Clips text to width columns (one column per code point is close enough
 *  for what hydra prints), with an ellipsis. */
export function clip(s: string, width: number): string {
	if (width <= 0) return "";
	const chars = Array.from(s);
	if (chars.length <= width) return s;
	if (width === 1) return "…";
	return chars.slice(0, width - 1).join("") + "…";
}

export function pad(s: string, width: number): string {
	const c = clip(s, width);
	return c + " ".repeat(Math.max(0, width - Array.from(c).length));
}

/** Word-wraps text to width columns. A line's own indentation carries over
 *  to the lines it wraps onto, plus more. */
export function wrap(text: string, width: number, more = "  "): string[] {
	const out: string[] = [];
	for (const line of text.split("\n")) {
		const indent = /^\s*/.exec(line)![0];
		const words = line.slice(indent.length).split(" ");
		let cur = indent;
		let fresh = true;
		for (const w of words) {
			const len = Array.from(cur).length;
			if (!fresh && len + 1 + Array.from(w).length > width) {
				out.push(cur);
				cur = indent + more + w;
			} else {
				cur = fresh ? cur + w : cur + " " + w;
			}
			fresh = false;
		}
		out.push(cur);
	}
	return out.flatMap((l) => {
		// A single word longer than the width: cut it.
		const chars = Array.from(l);
		const parts: string[] = [];
		for (let i = 0; i < chars.length || i === 0; i += Math.max(1, width)) parts.push(chars.slice(i, i + width).join(""));
		return parts;
	});
}

/** Where a service's changes are: the deepest folder all its changed files
 *  share, and what in it changed, each a folder or a file, in order of
 *  name. services/core/cmd/api/main.go alone is services/core/cmd/api/ and
 *  the file main.go; with services/core/internal/x.go too, services/core/
 *  and the folders cmd and internal. */
export function changedIn(files: string[]): { dir: string; items: Array<{ name: string; folder: boolean }> } {
	const split = files.map((f) => f.split("/"));
	const dirs = split.map((p) => p.slice(0, -1));
	let depth = 0;
	while (dirs.every((d) => d.length > depth && d[depth] === dirs[0]![depth])) depth++;
	const seen = new Map<string, boolean>();
	for (const p of split) seen.set(p[depth]!, seen.get(p[depth]!) || p.length > depth + 1);
	const items = [...seen].map(([name, folder]) => ({ name, folder })).sort((a, b) => a.name.localeCompare(b.name));
	return { dir: split[0]!.slice(0, depth).map((d) => d + "/").join(""), items };
}

/** One line of at most width: the paths in it shortened in the middle
 *  (apps/web/…/activity.constants.ts), longest first, keeping the file's
 *  name, and only then the end cut with …. Why a service runs is one fact:
 *  wrapped, a long path broke into pieces on lines of their own. */
export function fitPaths(text: string, width: number): string {
	const n = (s: string) => Array.from(s).length;
	const words = text.split(" ");
	for (;;) {
		const line = words.join(" ");
		if (n(line) <= width) return line;
		// the longest path that can still give up a directory
		let at = -1;
		for (let i = 0; i < words.length; i++) {
			const parts = words[i]!.split("/");
			if (parts.length > 2 && !(parts.length === 3 && parts[1] === "…") && (at < 0 || n(words[i]!) > n(words[at]!))) at = i;
		}
		if (at < 0) break;
		const parts = words[at]!.split("/");
		// drop the directory just before the file (or the …), keep the first
		const rest = parts.slice(1, -1).filter((d) => d !== "…");
		rest.pop();
		words[at] = [parts[0], ...rest, "…", parts[parts.length - 1]].join("/");
	}
	const chars = Array.from(words.join(" "));
	return chars.length <= width ? chars.join("") : chars.slice(0, Math.max(0, width - 1)).join("") + "…";
}

/** A path with the home directory as ~. */
export function tildePath(p: string, home: string | undefined): string {
	if (home && (p === home || p.startsWith(home + "/"))) return "~" + p.slice(home.length);
	return p;
}

/** The services main runs, or would run, split into those that are on and
 *  those switched off (disabled until someone starts them). */
export function mainServices(e: Entry): { on: Service[]; off: Service[] } {
	const off = e.up ? e.services.filter((s) => s.health === "off") : [];
	return { on: e.services.filter((s) => !off.includes(s)), off };
}

// ── Pull requests ─────────────────────────────────────────────────────

/** Nothing is left to do on the branch: its PR is merged or closed. */
export function prDone(pr?: PR): boolean {
	return pr?.state === "merged" || pr?.state === "closed";
}

/** A PR in a few columns, for the list: "#10265 draft", "#10265 ✗ checks". */
export function prShort(pr?: PR): string {
	if (!pr) return "";
	const num = pr.number ? `#${pr.number} ` : "";
	if (pr.state !== "open") return num + pr.state;
	if (pr.checks === "failing") return num + "✗ checks";
	if (pr.review === "changes requested") return num + "✗ review";
	if (pr.checks === "pending") return num + "◐ checks";
	if (pr.review === "approved") return num + "approved";
	if (pr.checks === "passing") return num + "✓";
	return num + "open";
}

/** The colour of a PR's state: GitHub's, more or less. */
export function prColor(pr?: PR): string | undefined {
	if (!pr) return undefined;
	if (pr.state === "merged") return "magenta";
	if (pr.state === "closed") return "red";
	if (pr.state === "draft") return "gray";
	if (pr.checks === "failing" || pr.review === "changes requested") return "red";
	if (pr.checks === "pending") return "yellow";
	return "green";
}

/** A PR on one line: "#10265 open · review required · checks passing". */
export function prLong(pr: PR): string {
	const parts = [pr.number ? `#${pr.number} ${pr.state}` : pr.mergedLocally ? "merged: its commits are in origin's main" : pr.state];
	if (!prDone(pr)) {
		if (pr.review) parts.push(pr.review);
		if (pr.checks) parts.push(`checks ${pr.checks}`);
	}
	return parts.join(" · ");
}

/** The widest PR text in the list, which is the PR column's width. */
export function prWidth(list: Entry[]): number {
	return Math.max(0, ...list.map((e) => Array.from(prShort(e.pr)).length));
}

/** The worktrees and branches of a merged or closed PR. */
export function donePRCount(list: Entry[]): { count: number; closed: boolean } {
	const done = list.filter((e) => prDone(e.pr));
	return { count: done.length, closed: done.some((e) => e.pr!.state === "closed") };
}
