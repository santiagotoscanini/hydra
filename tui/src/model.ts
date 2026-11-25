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

/** What the list says about a checkout's stack. */
export function stackState(e: Entry): { text: string; color?: string } {
	if (e.error) return { text: "error", color: "red" };
	if (e.pending) return { text: "…", color: "gray" };
	const blocking = e.problems.filter((p) => p.blocks).length;
	if (!e.up) {
		if (e.kind === "main") return { text: "DOWN", color: "red" };
		return { text: "down", color: "gray" };
	}
	const svcs = runsHere(e);
	if (svcs.some((s) => s.health === "failed")) return { text: "failing", color: "red" };
	if (svcs.some((s) => s.health === "starting")) return { text: "starting", color: "yellow" };
	if (blocking > 0) return { text: `up ✗${blocking}`, color: "red" };
	return { text: "up", color: "green" };
}

/** How a checkout stands against main, in a few words for the list. */
export function vsMain(e: Entry): { text: string; color?: string } {
	if (e.kind === "main") {
		const parts: string[] = [];
		if (e.behind > 0) parts.push(`${e.behind}↓ upstream`);
		if (e.dirty > 0) parts.push(`${e.dirty} uncommitted`);
		return { text: parts.join(" · "), color: parts.length ? "yellow" : undefined };
	}
	const parts: string[] = [];
	let color: string | undefined;
	if (e.update?.conflicts?.length) {
		parts.push(`conflicts(${e.update.conflicts.length})`);
		color = "red";
	} else if (e.outOfStep.length > 0) {
		parts.push(e.mainBehind ? `main older: ${e.outOfStep.join(",")}` : `behind in ${e.outOfStep.join(",")}`);
		color = "yellow";
	}
	if (e.behind > 0) parts.push(`${e.behind}↓`);
	if (e.ahead > 0) parts.push(`${e.ahead}↑`);
	if (e.update?.note.startsWith("left alone: everything on it")) {
		parts.push("landed");
		color ??= "gray";
	}
	if (e.stack.length > 1) parts.push(`on ${e.stack[e.stack.length - 2]}`);
	return { text: parts.join(" "), color };
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
