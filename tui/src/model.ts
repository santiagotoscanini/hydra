// What the dashboard shows, worked out from a snapshot. Pure: no I/O.

import type { Health, Problem, Service, Snapshot, Update } from "./types.js";

/** One row of the list: the main checkout or a worktree. */
export interface Entry {
	kind: "main" | "worktree";
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
			outOfStep: w.outOfStep.map((k) => k.service),
			mainBehind: !!w.mainBehind,
			idleStopped: w.idleStopped?.after,
			farBehind: w.farBehind && { behind: w.farBehind.behind, conflicts: w.farBehind.conflicts ?? [] },
			memory: w.memory,
			pending: !!w.pending,
			error: w.error,
			current: s.current === w.root,
		});
	}
	return out;
}

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
	return e.kind === "worktree" && !e.error && !e.pending && !landed(e) && (e.behindParent > 0 || parentConflicts(e) > 0);
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
 * parent is the worktree whose branch it is updated from; the rest (main,
 * or not worked out yet) hang off main.
 */
export function tree(list: Entry[]): Node[] {
	const main = list.find((e) => e.kind === "main");
	const byBranch = new Map(list.filter((e) => e.kind === "worktree").map((e) => [e.branch, e]));
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

const plural = (n: number, one: string, many = one + "s") => `${n} ${n === 1 ? one : many}`;

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
	if (e.kind === "main") {
		const parts: string[] = [];
		if (e.behind > 0) parts.push(`${e.behind} behind upstream`);
		if (e.dirty > 0) parts.push(`${e.dirty} uncommitted`);
		return parts.length ? { texts: [parts.join(" · ")], color: "yellow" } : { texts: ["up to date"], dim: true };
	}
	const s = n.stale;
	const parent = e.parent ? shortBranch(e.parent) : "main";
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
			const from = shortBranch(s.from.branch);
			return { texts: [`${from} is behind`, `${from} behind`, "parent behind"], dim: true, mark: "↑ " };
		}
		case "current":
			if (e.outOfStep.length > 0 && e.mainBehind) return { texts: ["in step · main is older", "main is older"], color: "yellow" };
			return { texts: ["in step"], dim: true };
	}
}

/** The chain from main to a checkout, each stale branch with how far
 *  behind: "main → fla-709 (69 behind) → fla-710". */
export function chainText(n: Node, staleOf: (e: Entry) => Staleness): string {
	return [...n.chain, n.e]
		.map((e) => {
			const name = e.kind === "main" ? "main" : shortBranch(e.branch);
			const s = staleOf(e);
			if (s.kind !== "behind") return name;
			const what = [s.behind ? `${s.behind} behind` : "", s.conflicts ? plural(s.conflicts, "conflict") : ""].filter(Boolean);
			return `${name} (${what.join(", ")})`;
		})
		.join(" → ");
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
