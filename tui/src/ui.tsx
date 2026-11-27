// The dashboard's pieces. Every line is one terminal row: text is clipped,
// never wrapped, so the frame keeps its height at any width.

import React, { createContext, useContext, useEffect, useState } from "react";
import stringWidth from "string-width";
import { Box, Text } from "ink";
import {
	bytes,
	conflictCount,
	detailServices,
	displayName,
	healthGlyph,
	mainServices,
	pad,
	progress,
	stateOf,
	tildePath,
	wrap,
	fitPaths,
	changedIn,
	badgeOf,
	labels,
	plural,
	firstToUpdate,
	labelBeside,
	prColor,
	prDone,
	prLong,
	prShort,
	type Entry,
	type Node,
	type Staleness,
} from "./model.js";
import type { PRSource, Service, Tunnel } from "./types.js";
import { themeFor, type Theme } from "./theme.js";

// ── Colours ───────────────────────────────────────────────────────────

/** The terminal's theme (theme.ts): the selection's background, and yellow. */
export const ThemeContext = createContext<Theme>(themeFor());

/** A named colour as this theme draws it: only yellow differs. */
function usePaint(): (c?: string) => string | undefined {
	const t = useContext(ThemeContext);
	return (c) => (c === "yellow" ? t.yellow : c);
}

/** A word's colour: gray marks things (a glyph, a border), never words,
 *  which it makes too faint on many palettes. */
const wordColor = (c?: string) => (c === "gray" ? undefined : c);

// ── Lines made of segments ────────────────────────────────────────────

/** A run of text in one style. spin draws a spinner (one column) instead. */
export interface Seg {
	text: string;
	color?: string;
	bold?: boolean;
	dim?: boolean;
	spin?: boolean;
	/** a background of its own (a key drawn as a pill) */
	bg?: string;
	/** what a click on it does: open a URL, or choose a checkout (its root) */
	link?: { url?: string; root?: string };
	/** a key a click on it presses (the footer's) */
	key?: string;
}

/** The segment of a row at column x, as Row draws it: for a click. */
export function segAt(segs: Seg[], width: number, x: number): Seg | undefined {
	let at = 0;
	for (const s of fit(segs, width)) {
		const n = s.spin ? 1 : len(s.text);
		if (x >= at && x < at + n) return s;
		at += n;
	}
	return undefined;
}

/** What a click at column x of a row does: its segment's link, or a URL
 *  written in the text under it. */
export function linkAt(segs: Seg[], width: number, x: number): { url?: string; root?: string } | undefined {
	const s = segAt(segs, width, x);
	if (!s) return undefined;
	if (s.link) return s.link;
	// A URL in the text: the one under the click.
	let at = 0;
	for (const t of fit(segs, width)) {
		if (t === s) break;
		at += t.spin ? 1 : len(t.text);
	}
	for (const m of s.text.matchAll(/https?:\/\/\S+/g)) {
		const from = at + len(s.text.slice(0, m.index));
		if (x >= from && x < from + len(m[0])) return { url: m[0] };
	}
	return undefined;
}

// Columns, not characters: 📁 takes two.
const len = (s: string) => stringWidth(s);

/** Cuts segments to width columns, with an ellipsis where it cut. */
function fit(segs: Seg[], width: number): Seg[] {
	const out: Seg[] = [];
	let room = width;
	for (const s of segs) {
		if (room <= 0) break;
		const n = s.spin ? 1 : len(s.text);
		if (n <= room) {
			out.push(s);
			room -= n;
			continue;
		}
		const chars = Array.from(s.text);
		out.push({ ...s, text: room === 1 ? "…" : chars.slice(0, room - 1).join("") + "…" });
		room = 0;
	}
	return out;
}

/** One row of exactly width columns. bg colours all of it (the selection). */
export function Row({ segs, width, bg }: { segs: Seg[]; width: number; bg?: string }) {
	const paint = usePaint();
	const cut = fit(segs, width);
	const used = cut.reduce((n, s) => n + (s.spin ? 1 : len(s.text)), 0);
	return (
		<Box width={width} height={1} overflow="hidden">
			<Text wrap="truncate-end">
				{cut.map((s, i) =>
					s.spin ? (
						<Spinner key={i} color={s.color} bg={bg} />
					) : (
						<Text key={i} color={paint(s.color)} bold={s.bold} dimColor={s.dim} backgroundColor={paint(s.bg) ?? bg}>
							{s.text}
						</Text>
					),
				)}
				<Text backgroundColor={bg}>{" ".repeat(Math.max(0, width - used))}</Text>
			</Text>
		</Box>
	);
}

/** One row, clipped to the width (for whole strings of one style). */
export function Line({ children, width }: { children?: React.ReactNode; width?: number }) {
	return (
		<Box width={width} height={1} overflow="hidden">
			<Text wrap="truncate-end">{children ?? " "}</Text>
		</Box>
	);
}

// ── Panels ────────────────────────────────────────────────────────────

/** A box with rounded corners and its title in the top border. */
export function Panel({
	title,
	color = "gray",
	width,
	height,
	children,
}: {
	title: Seg[];
	color?: string;
	width: number;
	height: number;
	children?: React.ReactNode;
}) {
	const paint = usePaint();
	const inner = Math.max(0, width - 2);
	const head = fit([{ text: "─ ", color }, ...title, { text: " " }], Math.max(0, inner - 1));
	const headLen = head.reduce((n, s) => n + (s.spin ? 1 : len(s.text)), 0);
	return (
		<Box flexDirection="column" width={width} height={height}>
			<Box height={1} width={width} overflow="hidden">
				<Text wrap="truncate-end">
					<Text color={paint(color)}>╭</Text>
					{head.map((s, i) =>
						s.spin ? (
							<Spinner key={i} color={s.color} />
						) : (
							<Text key={i} color={paint(s.color)} bold={s.bold} dimColor={s.dim}>
								{s.text}
							</Text>
						),
					)}
					<Text color={paint(color)}>{"─".repeat(Math.max(0, inner - headLen)) + "╮"}</Text>
				</Text>
			</Box>
			<Box
				borderStyle="round"
				borderTop={false}
				borderColor={paint(color)}
				flexDirection="column"
				width={width}
				height={Math.max(1, height - 1)}
				overflow="hidden"
			>
				{children}
			</Box>
		</Box>
	);
}

// ── The list ──────────────────────────────────────────────────────────

/** The width of the state and badge columns, from the list's inner width. */
export function listColumns(width: number): { state: number; badge: number } {
	return { state: 13, badge: width >= 60 ? 26 : width >= 50 ? 22 : 18 };
}

/** The columns of a row at a depth: the name shrinks with the connectors
 *  before it, down to a point, and then the badge gives up room. */
export function rowColumns(width: number, lead: number): { name: number; state: number; badge: number } {
	const cols = listColumns(width);
	const name = width - 4 - lead - cols.state - cols.badge - 2;
	const badge = name >= 10 ? cols.badge : Math.max(12, cols.badge - (10 - name));
	return { name: Math.max(4, width - 4 - lead - cols.state - badge - 2), state: cols.state, badge };
}

export function ListRow({
	node,
	selected,
	width,
	busy,
	short,
	prW = 0,
}: {
	node: Node;
	selected: boolean;
	width: number;
	busy?: string;
	/** The PR column's width; 0: no PR column (none known, or no room). */
	prW?: number;
	/** What tells it apart from branches with the same ticket ("schema"),
	 *  shown after the ticket when the whole title doesn't fit. */
	short?: string;
}) {
	const { selectionBg } = useContext(ThemeContext);
	const e = node.e;
	const st = stateOf(e, busy);
	const badge = badgeOf(node);
	const full = displayName(e);
	const cols = rowColumns(width - (prW ? prW + 1 : 0), len(node.lead));
	const n = full.id && short && len(full.id) + 1 + len(full.title) > cols.name ? { id: full.id, title: short } : full;
	const bg = selected ? selectionBg : undefined;
	// On the selection's background the words are the terminal's own text
	// colour, which reads on it in any palette; the glyph keeps the state's
	// colour. Without a background (the theme isn't known) the name is cyan.
	const on = (c?: string) => (bg ? undefined : wordColor(c));
	// A branch with no checkout is drawn muted: nothing runs there.
	const muted = e.kind === "branch" && !bg;
	// ⇄: the tunnel's requests come here (a worktree that holds one).
	const held: Seg[] = e.kind === "worktree" && e.tunnels.length ? [{ text: "⇄ ", color: on("magenta"), bold: true }] : [];
	const name: Seg[] = n.id
		? [
				...held,
				{ text: n.id, bold: !muted, dim: muted, color: selected && !bg ? "cyan" : undefined },
				{ text: " " + n.title, bold: selected, dim: muted },
			]
		: [...held, { text: n.title, bold: !muted, dim: muted, color: selected ? on("cyan") : e.kind === "main" ? "magenta" : undefined }];
	const nameFit = fit(name, cols.name);
	const nameLen = nameFit.reduce((k, s) => k + len(s.text), 0);
	// The badge right-aligned: the longest way of saying it that fits.
	const markLen = badge.mark ? len(badge.mark) : 0;
	const room = cols.badge - markLen;
	const text = badge.texts.find((t) => len(t) <= room) ?? pad(badge.texts[badge.texts.length - 1]!, room);
	const segs: Seg[] = [
		{ text: selected ? "▌" : " ", color: "cyan" },
		{ text: node.lead, color: "gray" },
		st.spin ? { text: "", spin: true, color: st.color } : { text: onDisk(e, st.glyph), color: st.color },
		{ text: " " },
		...nameFit,
		{ text: " ".repeat(cols.name - nameLen + 1) },
		{ text: pad(st.word, cols.state), color: on(st.color), bold: !!bg },
		...(prW ? [{ text: " " + pad(prShort(e.pr), prW), color: on(prColor(e.pr)), bold: !!bg || prDone(e.pr) }] : []),
		{ text: " " + " ".repeat(Math.max(0, room - len(text))) },
		...(badge.mark ? [{ text: badge.mark, color: "yellow" }] : []),
		{ text, color: on(badge.color), bold: badge.bold || !!bg, dim: badge.dim && !bg },
	];
	return <Row segs={segs} width={width} bg={bg} />;
}

/** The list's glyph: a worktree (or main) is a box, its files on disk, in
 *  the colour of its state; a branch with no worktree is ⎇. Failing,
 *  planning and can't plan keep their own glyphs. */
export function onDisk(e: Entry, glyph: string): string {
	return e.kind !== "branch" && (glyph === "●" || glyph === "○" || glyph === "◌") ? "▣" : glyph;
}

// ── The detail of the selected checkout ───────────────────────────────

function section(icon: string, label: string, color: string, more?: Seg[]): Seg[] {
	return [{ text: icon + " ", color, bold: true }, { text: label, bold: true }, ...(more ?? [])];
}

/** Where a service answers: its address by name, its link, or its port. */
function whereOf(s: Service): string {
	return s.address ?? s.url ?? (s.port ? `:${s.port}` : "");
}

function serviceRow(s: Service, nameW: number, whereW: number, stateW: number): Seg[] {
	const g = healthGlyph(s.health);
	const where = pad(whereOf(s), whereW);
	return [
		{ text: "  " },
		s.health === "starting" ? { text: "", spin: true, color: "yellow" } : { text: g.glyph, color: g.color },
		{ text: " " + pad(s.name, nameW) + " " },
		{ text: pad(s.state, stateW), color: s.health === "failed" ? "red" : s.health === "up" ? "green" : undefined },
		{ text: whereW ? " " + where : "", color: "cyan" },
		{ text: s.memory ? `  ${bytes(s.memory)}` : "" },
	];
}

/** Where a service changed, as rows: the folder its files share, then up
 *  to three folders or files in it, one a row, and how many more.
 *  "changed: apps/api/ 📄 orders.go" when it is one. What comes before the
 *  first file in why ("changed in core") is kept. */
export function changedRows(why: string, files: string[], width: number): Seg[][] {
	const head = why.includes(": ") ? why.slice(0, why.indexOf(": ")) : "changed";
	const { dir, items } = changedIn(files);
	const icon = (it: { folder: boolean }) => (it.folder ? "📁 " : "📄 ");
	const name = (it: { name: string; folder: boolean }) => it.name + (it.folder ? "/" : "");
	if (items.length === 1) {
		const it = items[0]!;
		const tail = icon(it) + name(it);
		return [[{ text: fitPaths(`${head}: ${dir}`.trimEnd(), Math.max(10, width - len(tail) - 1)) + " ", dim: true }, { text: icon(it) }, { text: name(it), dim: true }]];
	}
	const rows: Seg[][] = [[{ text: fitPaths(`${head}: ${dir}`.trimEnd(), width), dim: true }]];
	for (const it of items.slice(0, 3)) rows.push([{ text: "  " + icon(it) }, { text: name(it), dim: true }]);
	if (items.length > 3) rows.push([{ text: `  … ${items.length - 3} more`, dim: true }]);
	return rows;
}

/** The detail panel's title: the checkout's name and its state. */
export function detailTitle(e: Entry, busy?: string): Seg[] {
	const st = stateOf(e, busy);
	const n = displayName(e);
	return [
		...(n.id ? [{ text: n.id + " ", bold: true, color: "cyan" }] : []),
		{ text: n.title || e.name, bold: true, color: e.kind === "main" ? "magenta" : undefined },
		{ text: "  " },
		st.spin ? { text: "", spin: true, color: st.color } : { text: st.glyph, color: st.color },
		{ text: " " + st.word, color: wordColor(st.color), bold: true },
	];
}

/** The detail as rows of segments, so the caller can show the ones that
 *  fit: what runs in the checkout and why, in a fixed order. A worktree's:
 *  its PR, its chain of branches up to main, what runs here and the files
 *  that made it, what it takes from main, its problems. Main's: what it
 *  runs, its upstream, its problems. A branch with no checkout's: its PR,
 *  its chain, and that nothing runs it. */
export function detailRows(
	e: Entry,
	width: number,
	opts: {
		home?: string;
		busy?: string;
		problems?: boolean;
		node?: Node;
		staleOf?: (e: Entry) => Staleness;
		/** where the PRs come from: absent from older hydras */
		prs?: PRSource;
		/** the worktrees stacked on a branch with no checkout, which run its code */
		via?: Entry[];
		/** the stack's tunnels, each with what its holder is called */
		tunnels?: TunnelHeld[];
	} = {},
): Seg[][] {
	const out: Seg[][] = [];
	const add = (...segs: Seg[]) => out.push(segs);
	const blank = () => out.push([]);

	// Who it is: its name and state are in the panel's title (detailTitle).
	if (e.idleStopped && !e.up) add({ text: `nobody used it for ${e.idleStopped}, so hydra stopped it: u starts it`, color: "blue" });
	else if (e.kind === "main" && !e.up && !e.pending && !opts.busy) add({ text: "not running: the worktrees that use main's services need it", color: "red" });
	// Who it is, one fact a line, each under a label.
	const field = (label: string, ...value: Seg[]) => add({ text: pad(label, 8), dim: true }, ...fit(value, width - 8));
	field("branch", { text: e.branch });
	if (e.kind === "branch") field("folder", { text: "none: a branch only, no worktree", dim: true });
	else field("folder", { text: fitPaths(tildePath(e.root, opts.home), width - 8) });
	if (e.commit) field("commit", { text: e.commit + " ", bold: true }, { text: e.subject });
	if (e.memory) field("memory", { text: bytes(e.memory) });

	if (e.kind === "main") {
		blank();
		out.push(...mainServiceRows(e, width));
		const parts: string[] = [];
		if (e.behind > 0) parts.push(`${e.behind} commit${e.behind === 1 ? "" : "s"} behind its upstream (U pulls them)`);
		if (e.dirty > 0) parts.push(`${e.dirty} file${e.dirty === 1 ? "" : "s"} with uncommitted changes`);
		blank();
		if (parts.length) add(...section("⇅", "Upstream", "yellow", [{ text: "  " + parts.join(" · "), color: "yellow" }]));
		else add(...section("⇅", "Upstream", "cyan", [{ text: "  up to date", dim: true }]));
		out.push(...tunnelRows(e, width, opts.tunnels));
		if (opts.problems) out.push(...problemSection(e, width));
		return out;
	}

	// 1. Its PR.
	const pr = prRows(e, opts.prs);
	if (pr.length) {
		blank();
		out.push(...pr);
	}
	if (e.error) {
		blank();
		for (const l of wrap(`can't plan it: ${e.error}`, width)) add({ text: l, color: "red" });
		return out;
	}
	if (e.pending) {
		blank();
		add({ text: "", spin: true, color: "gray" }, { text: " working out what runs here…" });
		return out;
	}

	// 2. Its chain of branches up to main, and what to update first.
	blank();
	out.push(...chainRows(e, width, opts.node, opts.staleOf));

	// 3. What runs here, and why.
	blank();
	if (e.kind === "branch") {
		add(...section("◆", "Runs here", "gray", [{ text: "  nothing: no worktree has it checked out" }]));
		const via = (opts.via ?? []).map((w) => labelBeside(w.branch, e.branch));
		const text = via.length
			? `${via.join(", ")}, stacked on it, run${via.length === 1 ? "s" : ""} its code. m merges its parent into it without a checkout (hydra update --branch).`
			: "m merges its parent into it without a checkout (hydra update --branch).";
		for (const l of wrap(text, width - 2, "")) add({ text: "  " + l, dim: true });
		return out;
	}
	const { shown, hidden } = detailServices(e);
	const here = shown.filter((s) => s.runs === "here");
	const p = progress(e);
	add(...section("◆", "Runs here, and why", "cyan", [{ text: here.length ? (e.up ? `  ${p.ready}/${p.total} ready` : `  ${here.length}`) : "  nothing: everything comes from main" }]));
	const nameW = Math.min(20, Math.max(6, ...here.map((s) => len(s.name))));
	const whereW = Math.max(0, ...here.map((s) => len(whereOf(s))));
	const stateW = Math.min(24, Math.max(0, ...here.map((s) => len(s.state))));
	for (const s of here) {
		add(...serviceRow(s, nameW, whereW, stateW));
		if (s.files?.length)
			changedRows(s.why ?? "changed", s.files, width - 6).forEach((r, i) => add({ text: i ? "      " : "    ↳ ", color: "gray" }, ...r));
		else if (s.why) add({ text: "    ↳ ", color: "gray" }, { text: fitPaths(s.why, width - 6), dim: true });
	}

	// 4. What it takes from main, on one line.
	const fromMain = shown.filter((s) => s.runs !== "here");
	if (fromMain.length || hidden) out.push(fromMainRow(fromMain, hidden, width));
	out.push(...tunnelRows(e, width, opts.tunnels));

	// 5. What is wrong, with the keys that fix it.
	if (opts.problems) out.push(...problemSection(e, width));
	return out;
}

/** A tunnel, and what its holder is called in the dashboard ("main" for
 *  main). */
export interface TunnelHeld {
	t: Tunnel;
	holder: string;
}

/** "⇄ Tunnel  hooks https://hooks.example.com comes here" for each tunnel:
 *  here, or where it goes instead. */
function tunnelRows(e: Entry, width: number, tunnels?: TunnelHeld[]): Seg[][] {
	const out: Seg[][] = [];
	for (const { t, holder } of tunnels ?? []) {
		const here = e.tunnels.includes(t.name);
		const what = `${t.name}${t.url ? " " + t.url : ""}`;
		const where = here ? (t.down ? "comes here, and gets a 502: its stack isn't running" : "comes here") : `goes to ${holder}`;
		const text = `  ${what} ${where}`;
		const room = width - 9;
		const segs: Seg[] = [{ text: len(text) <= room ? text : `  ${t.name} ${where}`, color: here ? (t.down ? "red" : "magenta") : undefined, dim: !here }];
		out.push([], section("⇄", "Tunnel", here ? "magenta" : "gray", segs));
		if (!here && e.kind === "worktree") out.push([{ text: "  t takes it here (asks first)", dim: true }]);
		if (here && e.kind === "worktree") out.push([{ text: "  t gives it back to main (asks first)", dim: true }]);
		if (!here && e.kind === "main") out.push([{ text: "  t gives it back to main (asks first)", dim: true }]);
	}
	return out;
}

/** A branch's PR: its state, review and checks, and its link; when it is
 *  merged or closed, that nothing is left to do. Nothing when gh can't be
 *  asked (the help says why). */
function prRows(e: Entry, prs?: PRSource): Seg[][] {
	const pr = e.pr;
	if (!pr) {
		if (!prs?.at || prs.error) return [];
		return [section("◎", "PR", "gray", [{ text: "  none for this branch", dim: true }])];
	}
	const color = prColor(pr);
	const out: Seg[][] = [section("◎", "PR", color ?? "gray", [{ text: "  " + prLong(pr), color: wordColor(color), bold: prDone(pr) }])];
	if (prDone(pr)) out.push([{ text: "  " }, { text: `PR ${pr.state}: nothing left to do here`, color: "magenta", bold: true }]);
	if (pr.url) out.push([{ text: "  " }, { text: pr.url, color: "cyan" }, { text: "  p opens it", dim: true }]);
	return out;
}

/** How it stands against its parent, simply. On main: one line, how far
 *  behind and ahead, and the files a merge would conflict in. In a stack:
 *  the chain from main down to it, one branch a line, the ones in step
 *  between folded into one, and which to update first. */
function chainRows(e: Entry, width: number, node?: Node, staleOf?: (e: Entry) => Staleness): Seg[][] {
	const out: Seg[][] = [];
	const add = (...segs: Seg[]) => out.push(segs);
	const of = staleOf ?? (() => ({ kind: "none" }) as Staleness);
	const what = (x: Entry): string => {
		const s = x === e && node ? node.stale : of(x);
		if (s.kind !== "behind") return "";
		const parentName = x.parent && x.stack.length > 1 ? labelBeside(x.parent, x.branch) : "main";
		return [s.behind ? `${s.behind} behind ${parentName}` : "", s.conflicts ? plural(s.conflicts, "conflict") : ""].filter(Boolean).join(" · ");
	};
	const conflicts = conflictCount(e);
	const stacked = !!node && node.chain.length > 1;
	const stale = node?.stale.kind === "behind" || node?.stale.kind === "inherits";
	const color = conflicts && !stacked ? "red" : stale || e.farBehind || e.outOfStep.length ? "yellow" : "cyan";

	if (!stacked) {
		const counts = [e.behind > 0 ? `${e.behind} behind` : "", e.ahead > 0 ? `${e.ahead} ahead` : ""].filter(Boolean).join(" · ");
		add(...section("⇅", "vs main", color, [{ text: "  " + (counts || "up to date") }]));
	} else {
		const first = firstToUpdate(node!);
		const title = first ? `${labelBeside(first.branch, e.branch)} needs an update first` : "every branch is in step";
		add(...section("⇅", "Chain", color, [{ text: "  " + title, color: first ? "yellow" : undefined }]));
		const all = [...node!.chain.filter((x) => x.kind !== "main"), e];
		const names = labels(all.map((x) => x.branch));
		const nameW = Math.min(28, Math.max(...all.map((x) => len(names.get(x.branch)!))) + 2);
		const main = node!.chain.find((x) => x.kind === "main");
		add({ text: "  main", dim: true, link: main ? { root: main.root } : undefined });
		let quiet: Entry[] = [];
		const flush = () => {
			if (quiet.length > 2) {
				const span = `${names.get(quiet[0]!.branch)} … ${names.get(quiet[quiet.length - 1]!.branch)}`;
				add({ text: "  │ ", color: "gray" }, { text: pad(span, nameW + 2) }, { text: `in step (${quiet.length})`, dim: true });
			} else for (const q of quiet) add({ text: "  │ ", color: "gray" }, { text: pad(names.get(q.branch)!, nameW + 2), link: { root: q.root } }, { text: "in step", dim: true });
			quiet = [];
		};
		for (const x of all) {
			const w = what(x);
			if (!w && x !== e) {
				quiet.push(x);
				continue;
			}
			flush();
			const here = x === e;
			add(
				{ text: here ? "  └ " : "  │ ", color: "gray" },
				{ text: pad(names.get(x.branch)!, nameW + 2), bold: here, link: { root: x.root } },
				{ text: w || "in step", color: w ? (w.includes("conflict") ? "red" : "yellow") : undefined, dim: !w },
				{ text: x === first ? "   ← start here" : here ? "   ← this one" : "", color: x === first ? "yellow" : undefined, dim: x !== first },
			);
		}
		if (first) add({ text: `  m on ${labelBeside(first.branch, e.branch)}, or U for every branch, in order`, dim: true });
	}

	// What this branch itself would need: the files a merge conflicts in,
	// and the notes that aren't about the chain.
	const u = e.update;
	const files = u?.conflicts?.length ? u.conflicts : !stacked ? (e.farBehind?.conflicts ?? []) : [];
	if (files.length) {
		const into = u?.conflicts?.length && e.parent && e.stack.length > 1 ? labelBeside(e.parent, e.branch) : "main";
		add({ text: `  merging ${into} conflicts in:`, color: "red" });
		for (const f of files.slice(0, 3)) add({ text: "    📄 " }, { text: fitPaths(f, width - 7) });
		if (files.length > 3) add({ text: `    … ${files.length - 3} more`, dim: true });
	} else if (u?.hand) for (const l of wrap(u.note, width - 2)) add({ text: "  " + l, color: "yellow" });
	if (e.farBehind && !files.length && !stacked) add({ text: `  far behind: ${e.farBehind.behind} commits, and the merge only gets harder`, color: "yellow" });
	if (e.outOfStep.length && !files.length) {
		add({ text: e.mainBehind ? `  main is older than this branch in: ${e.outOfStep.join(", ")}` : `  main changed what this uses from it: ${e.outOfStep.join(", ")}`, color: "yellow" });
	}
	return out;
}

/** Main's services: what it runs, and what is switched off. */
function mainServiceRows(e: Entry, width: number): Seg[][] {
	const out: Seg[][] = [];
	const { on, off } = mainServices(e);
	const p = progress(e);
	out.push(section("◆", "Runs", "cyan", [{ text: e.up ? `  ${p.ready}/${p.total} ready` : `  ${on.length}, none running` }]));
	const nameW = Math.min(20, Math.max(6, ...on.map((s) => len(s.name))));
	const whereW = Math.max(0, ...on.map((s) => len(whereOf(s))));
	const stateW = Math.min(24, Math.max(0, ...on.map((s) => len(s.state))));
	for (const s of on) out.push(serviceRow(s, nameW, whereW, stateW));
	if (off.length) {
		for (const [i, l] of wrap(off.map((s) => s.name).join(", "), width - 16, "").entries())
			out.push([{ text: i === 0 ? "  switched off: " : " ".repeat(16) }, { text: l }]);
	}
	return out;
}

/** What a worktree takes from main, on one line:
 *  "◇ From main  ● api  ● db +3 · 12 unused". */
function fromMainRow(fromMain: Service[], hidden: number, width: number): Seg[] {
	const row: Seg[] = [{ text: "◇ ", color: "gray", bold: true }, { text: "From main", bold: true }];
	let used = 11;
	const tail = (rest: number) => (rest ? ` +${rest}` : "") + (hidden ? ` · ${hidden} unused` : "");
	for (const [i, s] of fromMain.entries()) {
		const g = healthGlyph(s.health);
		const label = s.runs === "routed" ? `${s.name} (routed)` : s.name;
		const w = 3 + len(label);
		if (used + w + len(tail(fromMain.length - i - 1)) > width) {
			row.push({ text: tail(fromMain.length - i), dim: true });
			return row;
		}
		row.push({ text: "  " }, { text: g.glyph, color: g.color }, { text: " " + label });
		used += w;
	}
	if (hidden) row.push({ text: tail(0), dim: true });
	return row;
}

/** The problems under a heading, last in the narrow layout's detail. */
function problemSection(e: Entry, width: number): Seg[][] {
	const out: Seg[][] = [[]];
	if (e.problems.length) out.push(section("!", e.problems.length === 1 ? "1 problem" : `${e.problems.length} problems`, problemsTitle(e).color));
	out.push(...problemRows(e, width));
	return out;
}

/** What is wrong with a checkout, each with its fix and the key that runs it. */
export function problemRows(e: Entry, width: number): Seg[][] {
	const out: Seg[][] = [];
	const add = (...segs: Seg[]) => out.push(segs);
	if (e.problems.length === 0) {
		if (!e.pending && !e.error)
			add(
				{ text: "✓ ", color: "green", bold: true },
				{ text: e.up || e.kind === "main" ? "nothing wrong" : e.kind === "branch" ? "nothing runs it: branch only, no worktree" : "nothing in the way: u starts it", color: "green" },
			);
		return out;
	}
	e.problems.forEach((p, i) => {
		if (i > 0) out.push([]);
		const mark = p.passing ? "◐" : p.blocks ? "✗" : "!";
		const color = p.passing ? "yellow" : p.blocks ? "red" : "yellow";
		const [first, ...rest] = p.text.split("\n");
		wrap(first!, width - 2).forEach((l, j) => add({ text: j === 0 ? `${mark} ` : "  ", color, bold: true }, { text: l }));
		for (const r of rest) for (const l of wrap(r, width - 2)) add({ text: "  " + l });
		if (p.fix) {
			const lines = wrap(p.fix, width - 6 - (p.run ? 13 : 0), "");
			lines.forEach((l, j) =>
				add(
					{ text: j === 0 ? "  fix " : "      ", dim: true },
					{ text: l, color: "cyan" },
					...(p.run && j === lines.length - 1 ? [{ text: "   " }, { text: "f", color: "cyan", bold: true }, { text: " runs it", dim: true }] : []),
				),
			);
		}
	});
	return out;
}

/** The problems panel's title: how many, coloured by the worst. */
export function problemsTitle(e: Entry): { title: Seg[]; color: string } {
	const n = e.problems.length;
	if (n === 0) return { title: [{ text: "Problems", bold: true, color: undefined }], color: "gray" };
	const color = e.problems.some((p) => p.blocks && !p.passing) ? "red" : "yellow";
	return { title: [{ text: "Problems ", bold: true, color: undefined }, { text: String(n), color, bold: true }], color };
}

// ── Overlays, drawn in the detail's place ─────────────────────────────

export function ConfirmBody({ lines: raw, width, height }: { lines: string[]; width: number; height: number }) {
	const lines = raw.flatMap((l) => wrap(l, width));
	const room = Math.max(0, height - 2);
	const shown = lines.length > room ? [...lines.slice(0, Math.max(0, room - 1)), `… ${lines.length - room + 1} more lines`] : lines;
	return (
		<Box flexDirection="column" height={height}>
			{shown.map((l, i) => (
				<Row key={i} width={width} segs={[{ text: l }]} />
			))}
			<Box flexGrow={1} />
			<Row
				width={width}
				segs={[
					{ text: " y ", color: "black", bg: "yellow", bold: true, key: "y" },
					{ text: " yes, do it", key: "y" },
					{ text: "    " },
					{ text: "n", color: "cyan", bold: true, key: "n" },
					{ text: " / ", dim: true, key: "n" },
					{ text: "esc", color: "cyan", bold: true, key: "n" },
					{ text: " no", dim: true, key: "n" },
				]}
			/>
		</Box>
	);
}

/** The first item a picker shows: the chosen one kept in the middle. */
export function pickerStart(index: number, count: number, height: number): number {
	const room = Math.max(1, height - 1);
	return Math.max(0, Math.min(index - Math.floor(room / 2), count - room));
}

export function PickerBody({ items, index, width, height }: { items: string[]; index: number; width: number; height: number }) {
	const { selectionBg } = useContext(ThemeContext);
	const room = Math.max(1, height - 1);
	const start = pickerStart(index, items.length, height);
	return (
		<Box flexDirection="column" height={height}>
			{items.slice(start, start + room).map((it, i) => {
				const sel = start + i === index;
				return <Row key={it + i} width={width} bg={sel ? selectionBg : undefined} segs={[{ text: sel ? "▌ " : "  ", color: "cyan" }, { text: it, bold: sel }]} />;
			})}
			<Box flexGrow={1} />
			<Row width={width} segs={[{ text: "↑↓", color: "cyan", bold: true }, { text: " choose  ", dim: true }, { text: "⏎", color: "cyan", bold: true }, { text: " ok  ", dim: true }, { text: "esc", color: "cyan", bold: true }, { text: " cancel", dim: true }]} />
		</Box>
	);
}

/** The output's lines, wrapped: back is how many it is scrolled up from the end. */
export function outputLines(text: string, width: number): string[] {
	return wrap(text.replace(/\s+$/, "") || "(it printed nothing)", width);
}

export function OutputBody({ text, width, height, back = 0 }: { text: string; width: number; height: number; back?: number }) {
	const lines = outputLines(text, width);
	const room = Math.max(0, height - 1);
	const end = lines.length - Math.min(back, Math.max(0, lines.length - room));
	const shown = lines.slice(Math.max(0, end - room), end);
	return (
		<Box flexDirection="column" height={height}>
			{shown.map((l, i) => (
				<Row key={i} width={width} segs={[{ text: l }]} />
			))}
			<Box flexGrow={1} />
			<Row width={width} segs={[{ text: "esc", color: "cyan", bold: true, key: "esc" }, { text: " closes", dim: true, key: "esc" }, ...(lines.length > room ? [{ text: "   ↑↓ or the wheel scroll", dim: true }] : [])]} />
		</Box>
	);
}

// Each key runs the hydra command an agent would, in that checkout.
export const KEYS: Array<[string, string]> = [
	["↑↓ j k", "choose a checkout"],
	["⏎", "open its stack: process-compose attach"],
	["", "  F10 or ctrl-c, then Yes, comes back; the stack keeps running"],
	["u", "start its stack: hydra up -D"],
	["d", "stop its stack: hydra down (asks first)"],
	["r", "restart a service: hydra process restart NAME"],
	["l", "follow a service's logs: hydra process logs NAME -f; ctrl-c ends"],
	["o", "open a service's address by name in the browser (x-hydra hosts)"],
	["p", "open its pull request in the browser"],
	["f", "run the fix of one of its problems (asks first)"],
	["m", "merge its parent (main, or the branch it is stacked on) into it: hydra update --here (asks first)"],
	["", "  on a branch with no worktree (⎇): hydra update --branch NAME"],
	["U", "update all: hydra update, which pulls main (dry run, then asks)"],
	["t", "take the tunnel (x-hydra tunnel) to it, on main give it back: hydra tunnel take (asks first)"],
	["R", "read everything again now"],
	["pgup pgdn", "scroll the detail"],
	["mouse", "click a checkout to choose it, a key in the footer to press it, a URL to open it;"],
	["", "  the wheel scrolls what is under it; drag a panel's border to resize it"],
	["", "  shift (option in iTerm2) and drag: select text as usual"],
	["?", "this help"],
	["q", "quit; every stack keeps running"],
	["▣", "a worktree: its files on disk, where its services run"],
	["", "  green up · grey down · blue stopped by hydra, unused for x-hydra idle_stop"],
	["⠋ ✗", "starting (ready/total) or another action under way · failing"],
	["⎇", "branch only: a stack's branch with no worktree; nothing runs it, only m applies"],
	["⇄", "holds the tunnel: its requests (webhooks) come to this worktree"],
	["├─ └─", "a branch under the one it is stacked on; main is the root"],
	["in step", "has every commit of its parent, and so does every branch above it"],
	["12 behind", "commits its parent has that it doesn't; bold: the chain goes stale here"],
	["↑ X behind", "in step with its parent, but X above it is behind: update X first"],
	["conflicts", "files merging its parent in would conflict in: merge and resolve by hand"],
	["#12 ✓", "its PR, from gh: ✓ checks pass · ✗ checks or review · ◐ checks run · draft"],
	["PR merged", "its PR is merged or closed: nothing left to do there"],
];

/** Where the PR column comes from, for the help: gh, or why there is none. */
export function prSourceLine(prs?: PRSource, now = Date.now()): string {
	if (!prs) return "no PR status: this hydra doesn't read PRs";
	if (prs.error) return `no PR status: ${prs.error}`;
	if (!prs.at) return "PR status: asking gh…";
	const min = Math.max(0, Math.round((now - Date.parse(prs.at)) / 60000));
	return `PR status from gh, read ${min ? `${min} min ago` : "just now"}; again every 3 minutes, and on R`;
}

export function HelpBody({ width, height, prs, from = 0 }: { width: number; height: number; prs?: PRSource; from?: number }) {
	const start = Math.min(from, Math.max(0, KEYS.length - (height - 1)));
	return (
		<Box flexDirection="column" height={height}>
			<Row width={width} segs={[{ text: prSourceLine(prs), dim: true }]} />
			{KEYS.slice(start, start + Math.max(0, height - 1)).map(([k, what], i) => (
				<Row key={i} width={width} segs={[{ text: pad(k, 11), color: "cyan", bold: true }, { text: what }]} />
			))}
		</Box>
	);
}

// ── The footer: the keys that apply to the selection ──────────────────

export function footerKeys(e: Entry | undefined, tunnel = false): Array<[string, string]> {
	const keys: Array<[string, string]> = [];
	if (e?.kind === "branch") {
		// Nothing to start, stop or look at: only its merge, and its PR.
		if (!e.pending && !prDone(e.pr) && (e.behindParent > 0 || e.update?.action)) keys.push(["m", `merge ${e.parent ? labelBeside(e.parent, e.branch) : "main"}`]);
		if (e.pr?.url) keys.push(["p", "PR"]);
		return keys;
	}
	if (e && !e.pending && !e.error) {
		if (e.up) keys.push(["⏎", "open"], ["d", "down"], ["r", "restart"]);
		else keys.push(["u", "up"]);
		if (e.up || detailServices(e).shown.some((s) => s.runs !== "here" && s.health !== "off")) keys.push(["l", "logs"]);
		if (e.services.some((s) => s.address)) keys.push(["o", "browse"]);
		if (e.problems.some((p) => p.run)) keys.push(["f", "fix"]);
		if (e.kind === "worktree" && !prDone(e.pr) && (e.behindParent > 0 || e.update?.action))
			keys.push(["m", `merge ${e.parent && e.stack.length > 1 ? labelBeside(e.parent, e.branch) : "main"}`]);
		if (e.pr?.url) keys.push(["p", "PR"]);
		if (tunnel) keys.push(["t", "tunnel"]);
	}
	return keys;
}

/** The footer. In an overlay, the overlay says its own keys. */
export function Footer(props: { e: Entry | undefined; width: number; overlay?: boolean; tunnel?: boolean }) {
	return <Row width={props.width} segs={footerSegs(props)} />;
}

/** The footer's segments; each key's carries the key a click presses. */
export function footerSegs({ e, width, overlay, tunnel }: { e: Entry | undefined; width: number; overlay?: boolean; tunnel?: boolean }): Seg[] {
	const left = overlay ? [] : footerKeys(e, tunnel);
	const right: Array<[string, string]> = overlay ? [] : [["U", "update all"], ["?", "help"], ["q", "quit"]];
	const segsOf = (keys: Array<[string, string]>, sep: string): Seg[] =>
		keys.flatMap(([k, what], i) => [...(i ? [{ text: sep, dim: true }] : []), { text: k, color: "cyan", bold: true, key: k }, { text: " " + what, dim: true, key: k }]);
	const width_ = (segs: Seg[]) => segs.reduce((n, s) => n + len(s.text), 0);
	// Narrower and narrower until it fits: tighter gaps, then shorter words.
	const short = (keys: Array<[string, string]>) => keys.map(([k, w]): [string, string] => [k, w.split(" ")[0]!]);
	let l: Seg[] = [];
	let r: Seg[] = [];
	for (const [sep, cut] of [["  ·  ", false], ["  ", false], ["  ", true], [" ", true]] as const) {
		l = segsOf(cut ? short(left) : left, sep);
		r = segsOf(cut ? short(right) : right, sep);
		if (width_(l) + width_(r) + 3 <= width) break;
	}
	const gap = Math.max(1, width - 1 - width_(l) - width_(r));
	return [{ text: " " }, ...l, { text: " ".repeat(gap) }, ...r];
}

const FRAMES = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];

export function Spinner({ color = "yellow", bg }: { color?: string; bg?: string }) {
	const paint = usePaint();
	const [i, setI] = useState(0);
	useEffect(() => {
		const t = setInterval(() => setI((n) => (n + 1) % FRAMES.length), 100);
		return () => clearInterval(t);
	}, []);
	return (
		<Text color={paint(color)} backgroundColor={bg}>
			{FRAMES[i]}
		</Text>
	);
}
