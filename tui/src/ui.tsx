// The dashboard's pieces. Every line is one terminal row: text is clipped,
// never wrapped, so the frame keeps its height at any width.

import React, { useEffect, useState } from "react";
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
	shortBranch,
	stateOf,
	tildePath,
	vsMain,
	wrap,
	type Entry,
} from "./model.js";
import type { Service } from "./types.js";

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
}

const len = (s: string) => Array.from(s).length;

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
	const cut = fit(segs, width);
	const used = cut.reduce((n, s) => n + (s.spin ? 1 : len(s.text)), 0);
	return (
		<Box width={width} height={1} overflow="hidden">
			<Text wrap="truncate-end">
				{cut.map((s, i) =>
					s.spin ? (
						<Spinner key={i} color={s.color} bg={bg} />
					) : (
						<Text key={i} color={s.color} bold={s.bold} dimColor={s.dim} backgroundColor={s.bg ?? bg}>
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
	const inner = Math.max(0, width - 2);
	const head = fit([{ text: "─ ", color }, ...title, { text: " " }], Math.max(0, inner - 1));
	const headLen = head.reduce((n, s) => n + (s.spin ? 1 : len(s.text)), 0);
	return (
		<Box flexDirection="column" width={width} height={height}>
			<Box height={1} width={width} overflow="hidden">
				<Text wrap="truncate-end">
					<Text color={color}>╭</Text>
					{head.map((s, i) =>
						s.spin ? (
							<Spinner key={i} color={s.color} />
						) : (
							<Text key={i} color={s.color} bold={s.bold} dimColor={s.dim}>
								{s.text}
							</Text>
						),
					)}
					<Text color={color}>{"─".repeat(Math.max(0, inner - headLen)) + "╮"}</Text>
				</Text>
			</Box>
			<Box
				borderStyle="round"
				borderTop={false}
				borderColor={color}
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

export function ListRow({
	e,
	selected,
	width,
	busy,
	selectionBg,
}: {
	e: Entry;
	selected: boolean;
	width: number;
	busy?: string;
	selectionBg: string;
}) {
	const st = stateOf(e, busy);
	const vs = vsMain(e);
	const n = displayName(e);
	const cols = listColumns(width);
	const nameW = Math.max(6, width - 4 - cols.state - cols.badge - 2);
	const bg = selected ? selectionBg : undefined;
	const name: Seg[] = n.id
		? [
				{ text: n.id, bold: true, color: selected ? "cyan" : undefined },
				{ text: " " + n.title, bold: selected, dim: !selected },
			]
		: [{ text: n.title, bold: true, color: e.kind === "main" ? "magenta" : selected ? "cyan" : undefined }];
	const nameFit = fit(name, nameW);
	const nameLen = nameFit.reduce((k, s) => k + len(s.text), 0);
	const segs: Seg[] = [
		{ text: selected ? "▌" : " ", color: "cyan" },
		st.spin ? { text: "", spin: true, color: st.color } : { text: st.glyph, color: st.color },
		{ text: " " },
		...nameFit,
		{ text: " ".repeat(nameW - nameLen + 1) },
		{ text: pad(st.word, cols.state), color: st.color },
		{ text: " " },
		{ text: vs.text.length > cols.badge ? pad(vs.text, cols.badge) : vs.text.padStart(cols.badge), color: vs.color },
	];
	return <Row segs={segs} width={width} bg={bg} />;
}

// ── The detail of the selected checkout ───────────────────────────────

function section(icon: string, label: string, color: string, more?: Seg[]): Seg[] {
	return [{ text: icon + " ", color, bold: true }, { text: label, bold: true }, ...(more ?? [])];
}

/** Where a service answers: its address by name, its link, or its port. */
function whereOf(s: Service): string {
	return s.address ?? s.url ?? (s.port ? `:${s.port}` : "");
}

function serviceRow(s: Service, nameW: number, whereW: number, stateW: number, worktree: boolean): Seg[] {
	const g = healthGlyph(s.health);
	const where = pad(whereOf(s), whereW);
	return [
		{ text: "  " },
		s.health === "starting" ? { text: "", spin: true, color: "yellow" } : { text: g.glyph, color: g.color },
		{ text: " " + pad(s.name, nameW) + " " },
		{ text: pad(s.state, stateW), color: s.health === "failed" ? "red" : s.health === "up" ? "green" : undefined, dim: s.health === "off" || s.health === "done" },
		{ text: whereW ? " " + where : "", color: "cyan", dim: !s.address && !s.url },
		{ text: s.memory ? `  ${bytes(s.memory)}` : "", dim: true },
		{ text: worktree && s.why ? `  ${s.why}` : "", dim: true },
	];
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
		{ text: " " + st.word, color: st.color, bold: true },
	];
}

/** The detail as rows of segments, so the caller can show the ones that fit. */
export function detailRows(e: Entry, width: number, opts: { home?: string; busy?: string; problems?: boolean } = {}): Seg[][] {
	const out: Seg[][] = [];
	const add = (...segs: Seg[]) => out.push(segs);
	const blank = () => out.push([]);

	// Who it is: its name and state are in the panel's title (detailTitle).
	const stateSegs: Seg[] = [];
	if (e.idleStopped && !e.up) stateSegs.push({ text: `nobody used it for ${e.idleStopped}, so hydra stopped it: u starts it`, color: "blue" });
	else if (e.kind === "main" && !e.up && !e.pending && !opts.busy) stateSegs.push({ text: "not running: the worktrees that use main's services need it", color: "red" });
	if (stateSegs.length) add(...stateSegs);
	add({ text: `${e.branch} · ${tildePath(e.root, opts.home)}`, dim: true }, { text: e.memory ? ` · ${bytes(e.memory)}` : "", dim: true });
	if (e.commit) add({ text: e.commit + " ", color: "yellow", dim: true }, { text: e.subject, dim: true });
	if (e.error) {
		blank();
		for (const l of wrap(`can't plan it: ${e.error}`, width)) add({ text: l, color: "red" });
		return out;
	}
	if (e.pending) {
		blank();
		add({ text: "", spin: true, color: "gray" }, { text: " working out what runs here…", dim: true });
		return out;
	}

	// What is wrong comes first: it is what to act on.
	if (opts.problems) {
		blank();
		if (e.problems.length) add(...section("!", e.problems.length === 1 ? "1 problem" : `${e.problems.length} problems`, problemsTitle(e).color));
		out.push(...problemRows(e, width));
	}

	// Against main.
	blank();
	if (e.kind === "main") {
		const parts: string[] = [];
		if (e.behind > 0) parts.push(`${e.behind} commit${e.behind === 1 ? "" : "s"} behind its upstream (U pulls them)`);
		if (e.dirty > 0) parts.push(`${e.dirty} file${e.dirty === 1 ? "" : "s"} with uncommitted changes`);
		if (parts.length) add(...section("⇅", "Upstream", "yellow", [{ text: "  " + parts.join(" · "), color: "yellow" }]));
		else out.pop();
	} else {
		const conflicts = conflictCount(e);
		const counts: string[] = [];
		if (e.behind > 0) counts.push(`${e.behind} behind`);
		if (e.ahead > 0) counts.push(`${e.ahead} ahead`);
		const color = conflicts ? "red" : e.farBehind || e.outOfStep.length ? "yellow" : "cyan";
		add(...section("⇅", "vs main", color, [{ text: "  " + (counts.join(" · ") || "up to date") }]));
		if (e.stack.length > 1) {
			add({ text: "  stacked on " }, { text: shortBranch(e.stack[e.stack.length - 2]!), bold: true }, { text: `  (main ← ${e.stack.map(shortBranch).join(" ← ")})`, dim: true });
		}
		const u = e.update;
		if (u) {
			const c = u.hand ? (u.conflicts?.length ? "red" : "yellow") : undefined;
			for (const l of wrap(`update: ${u.note}`, width - 2)) add({ text: "  " + l, color: c, dim: !c });
		}
		const files = u?.conflicts?.length ? u.conflicts : (e.farBehind?.conflicts ?? []);
		if (files.length) {
			const shown = files.slice(0, 3).map((f) => f.slice(f.lastIndexOf("/") + 1));
			add({ text: `  merging main conflicts in ${files.length} file${files.length === 1 ? "" : "s"}: `, color: "red" }, { text: shown.join(", ") + (files.length > 3 ? ` +${files.length - 3}` : ""), dim: true });
		}
		if (e.farBehind && !files.length) add({ text: `  far behind: ${e.farBehind.behind} commits, and the merge only gets harder`, color: "yellow" });
		if (e.outOfStep.length && !files.length) {
			add({ text: e.mainBehind ? `  main is older than this branch in: ${e.outOfStep.join(", ")}` : `  main changed what this uses from it: ${e.outOfStep.join(", ")}`, color: "yellow" });
		}
	}

	// Services.
	blank();
	if (e.kind === "main") {
		const { on, off } = mainServices(e);
		const p = progress(e);
		add(...section("◆", "Services", "cyan", [{ text: e.up ? `  ${p.ready}/${p.total} ready` : `  ${on.length}, none running`, dim: true }]));
		const nameW = Math.min(20, Math.max(6, ...on.map((s) => len(s.name))));
		const whereW = Math.max(0, ...on.map((s) => len(whereOf(s))));
		const stateW = Math.min(24, Math.max(0, ...on.map((s) => len(s.state))));
		for (const s of on) add(...serviceRow(s, nameW, whereW, stateW, false));
		if (off.length) {
			for (const [i, l] of wrap(off.map((s) => s.name).join(", "), width - 16, "").entries())
				add({ text: i === 0 ? "  switched off: " : " ".repeat(16), dim: true }, { text: l, dim: true });
		}
	} else {
		const { shown, hidden } = detailServices(e);
		const here = shown.filter((s) => s.runs === "here");
		const fromMain = shown.filter((s) => s.runs !== "here");
		const p = progress(e);
		add(...section("◆", "Runs here", "cyan", [{ text: here.length ? (e.up ? `  ${p.ready}/${p.total} ready` : `  ${here.length}`) : "  nothing: everything comes from main", dim: true }]));
		const nameW = Math.min(20, Math.max(6, ...here.map((s) => len(s.name))));
		const whereW = Math.max(0, ...here.map((s) => len(whereOf(s))));
		const stateW = Math.min(24, Math.max(0, ...here.map((s) => len(s.state))));
		for (const s of here) add(...serviceRow(s, nameW, whereW, stateW, true));
		if (fromMain.length || hidden) {
			blank();
			const items = fromMain.map((s) => {
				const g = healthGlyph(s.health);
				return { s, g, label: s.runs === "routed" ? `${s.name} (routed)` : s.name };
			});
			add(...section("◇", "From main", "gray", [{ text: hidden ? `  ${fromMain.length} used, ${hidden} more unused` : `  ${fromMain.length}`, dim: true }]));
			// As many per line as fit: "● api  ● supabase  ✓ migrate".
			let line: Seg[] = [{ text: " " }];
			let used = 1;
			for (const it of items) {
				const w = 3 + len(it.label);
				if (used + w > width && line.length > 1) {
					out.push(line);
					line = [{ text: " " }];
					used = 1;
				}
				line.push({ text: " " }, { text: it.g.glyph, color: it.g.color }, { text: " " + it.label });
				used += w;
			}
			if (line.length > 1) out.push(line);
		}
	}

	const addresses = e.services.filter((s) => s.address);
	if (addresses.length) {
		blank();
		add(...section("↗", "Addresses", "cyan", [{ text: "  o opens one", dim: true }]));
		for (const s of addresses) add({ text: "  " + pad(s.name, 10) + " " }, { text: s.address!, color: "cyan" });
	}

	return out;
}

/** What is wrong with a checkout, each with its fix and the key that runs it. */
export function problemRows(e: Entry, width: number): Seg[][] {
	const out: Seg[][] = [];
	const add = (...segs: Seg[]) => out.push(segs);
	if (e.problems.length === 0) {
		if (!e.pending && !e.error)
			add({ text: "✓ ", color: "green", bold: true }, { text: e.up || e.kind === "main" ? "nothing wrong" : "nothing in the way: u starts it", color: "green" });
		return out;
	}
	e.problems.forEach((p, i) => {
		if (i > 0) out.push([]);
		const mark = p.passing ? "◐" : p.blocks ? "✗" : "!";
		const color = p.passing ? "yellow" : p.blocks ? "red" : "yellow";
		const [first, ...rest] = p.text.split("\n");
		wrap(first!, width - 2).forEach((l, j) => add({ text: j === 0 ? `${mark} ` : "  ", color, bold: true }, { text: l }));
		for (const r of rest) for (const l of wrap(r, width - 2)) add({ text: "  " + l, dim: true });
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
					{ text: " y ", color: "black", bg: "yellow", bold: true },
					{ text: " yes, do it    " },
					{ text: "n", color: "cyan", bold: true },
					{ text: " / ", dim: true },
					{ text: "esc", color: "cyan", bold: true },
					{ text: " no", dim: true },
				]}
			/>
		</Box>
	);
}

export function PickerBody({ items, index, width, height, selectionBg }: { items: string[]; index: number; width: number; height: number; selectionBg: string }) {
	const room = Math.max(1, height - 1);
	const start = Math.max(0, Math.min(index - Math.floor(room / 2), items.length - room));
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

export function OutputBody({ text, width, height }: { text: string; width: number; height: number }) {
	const lines = wrap(text.replace(/\s+$/, "") || "(it printed nothing)", width);
	const room = Math.max(0, height - 1);
	const shown = lines.length > room ? lines.slice(lines.length - room) : lines;
	return (
		<Box flexDirection="column" height={height}>
			{shown.map((l, i) => (
				<Row key={i} width={width} segs={[{ text: l }]} />
			))}
			<Box flexGrow={1} />
			<Row width={width} segs={[{ text: "esc", color: "cyan", bold: true }, { text: " closes", dim: true }]} />
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
	["f", "run the fix of one of its problems (asks first)"],
	["m", "merge main into this worktree: hydra update --here (asks first)"],
	["U", "update all: hydra update, which pulls main (dry run, then asks)"],
	["R", "read everything again now"],
	["pgup pgdn", "scroll the detail"],
	["?", "this help"],
	["q", "quit; every stack keeps running"],
	["● ○ ◌", "up · down · stopped by hydra, unused for x-hydra idle_stop"],
	["⠋ ✗", "starting (ready/total) or another action under way · failing"],
	["behind", "commits main has that the branch doesn't; yellow: past behind_warn"],
	["on X", "stacked on branch X, so main reaches it through X"],
	["needs main", "main changed a service this worktree takes from it: m merges main"],
	["conflicts", "files merging main would conflict in: merge and resolve by hand"],
];

export function HelpBody({ width, height }: { width: number; height: number }) {
	return (
		<Box flexDirection="column" height={height}>
			{KEYS.slice(0, Math.max(0, height)).map(([k, what], i) => (
				<Row key={i} width={width} segs={[{ text: pad(k, 11), color: "cyan", bold: true }, { text: what }]} />
			))}
		</Box>
	);
}

// ── The footer: the keys that apply to the selection ──────────────────

export function footerKeys(e: Entry | undefined): Array<[string, string]> {
	const keys: Array<[string, string]> = [];
	if (e && !e.pending && !e.error) {
		if (e.up) keys.push(["⏎", "open"], ["d", "down"], ["r", "restart"]);
		else keys.push(["u", "up"]);
		if (e.up || detailServices(e).shown.some((s) => s.runs !== "here" && s.health !== "off")) keys.push(["l", "logs"]);
		if (e.services.some((s) => s.address)) keys.push(["o", "browse"]);
		if (e.problems.some((p) => p.run)) keys.push(["f", "fix"]);
		if (e.kind === "worktree" && (e.behind > 0 || e.update?.action)) keys.push(["m", "merge main"]);
	}
	return keys;
}

/** The footer. In an overlay, the overlay says its own keys. */
export function Footer({ e, width, overlay }: { e: Entry | undefined; width: number; overlay?: boolean }) {
	const left = overlay ? [] : footerKeys(e);
	const right: Array<[string, string]> = overlay ? [] : [["U", "update all"], ["?", "help"], ["q", "quit"]];
	const segsOf = (keys: Array<[string, string]>, sep: string): Seg[] =>
		keys.flatMap(([k, what], i) => [...(i ? [{ text: sep, dim: true }] : []), { text: k, color: "cyan", bold: true }, { text: " " + what, dim: true }]);
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
	return <Row width={width} segs={[{ text: " " }, ...l, { text: " ".repeat(gap) }, ...r]} />;
}

const FRAMES = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];

export function Spinner({ color = "yellow", bg }: { color?: string; bg?: string }) {
	const [i, setI] = useState(0);
	useEffect(() => {
		const t = setInterval(() => setI((n) => (n + 1) % FRAMES.length), 100);
		return () => clearInterval(t);
	}, []);
	return (
		<Text color={color} backgroundColor={bg}>
			{FRAMES[i]}
		</Text>
	);
}
