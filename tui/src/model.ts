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

/** How a checkout stands against main, in plain words for the list:
 *  "69 behind · 5 conflicts", "on fla-709 · up to date". */
export function vsMain(e: Entry): { text: string; color?: string } {
	const plural = (n: number, one: string, many = one + "s") => `${n} ${n === 1 ? one : many}`;
	if (e.kind === "main") {
		const parts: string[] = [];
		if (e.behind > 0) parts.push(`${e.behind} behind upstream`);
		if (e.dirty > 0) parts.push(`${e.dirty} uncommitted`);
		return parts.length ? { text: parts.join(" · "), color: "yellow" } : { text: "up to date", color: "gray" };
	}
	if (e.error || e.pending) return { text: "" };
	const parts: string[] = [];
	if (e.stack.length > 1) parts.push(`on ${shortBranch(e.stack[e.stack.length - 2]!)}`);
	const conflicts = conflictCount(e);
	const landed = e.update?.note.startsWith("left alone: everything on it");
	let color: string | undefined = "gray";
	// Stacked: what counts is its parent, which brings main with it.
	if (landed) parts.push("landed");
	else if (e.behind > 0 && e.stack.length <= 1) parts.push(`${e.behind} behind`);
	if (conflicts) {
		parts.push(plural(conflicts, "conflict"));
		color = "red";
	} else if (e.outOfStep.length > 0) {
		parts.push(e.mainBehind ? "main is older" : "needs main");
		color = "yellow";
	} else if (e.farBehind) {
		color = "yellow";
	}
	if (parts.length === 0) parts.push("up to date");
	return { text: parts.join(" · "), color };
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
