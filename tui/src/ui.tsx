// The dashboard's pieces. Every line is one terminal row: text is clipped,
// never wrapped, so the frame keeps its height at any width.

import React, { useEffect, useState } from "react";
import { Box, Text } from "ink";
import {
	bytes,
	clip,
	detailServices,
	healthGlyph,
	pad,
	stackState,
	vsMain,
	runsHere,
	wrap,
	type Entry,
} from "./model.js";

/** One row, clipped to the width. */
export function Line({ children, width }: { children?: React.ReactNode; width?: number }) {
	return (
		<Box width={width} height={1} overflow="hidden">
			<Text wrap="truncate-end">{children ?? " "}</Text>
		</Box>
	);
}

// ── The list ──────────────────────────────────────────────────────────

export interface Columns {
	name: number;
	branch: number; // 0: not shown
	slot: number;
	state: number;
	here: number;
	vs: number;
}

export function columns(width: number): Columns {
	const fixed = 3 + 5 + 10; // marker, slot, state (with their gaps)
	if (width >= 110) {
		const name = 22;
		const branch = 26;
		const here = 26;
		return { name, branch, slot: 4, state: 9, here, vs: Math.max(0, width - fixed - name - branch - here - 3) };
	}
	const name = width >= 90 ? 18 : 16;
	const here = width >= 90 ? 20 : 16;
	return { name, branch: 0, slot: 4, state: 9, here, vs: Math.max(0, width - fixed - name - here - 2) };
}

export function ListHeader({ cols }: { cols: Columns }) {
	return (
		<Line>
			<Text dimColor>
				{"   " +
					pad("CHECKOUT", cols.name) +
					" " +
					(cols.branch ? pad("BRANCH", cols.branch) + " " : "") +
					pad("SLOT", cols.slot) +
					" " +
					pad("STACK", cols.state) +
					" " +
					pad("RUNS HERE", cols.here) +
					" " +
					pad("VS MAIN", cols.vs)}
			</Text>
		</Line>
	);
}

export function ListRow({ e, selected, cols }: { e: Entry; selected: boolean; cols: Columns }) {
	const st = stackState(e);
	const vs = vsMain(e);
	const here =
		e.kind === "main"
			? e.up
				? `${e.services.filter((s) => s.health === "up" || s.health === "done").length}/${e.services.length} ok`
				: "everything"
			: runsHere(e)
					.map((s) => s.name)
					.join(",") || (e.pending ? "" : "-");
	const slot = e.kind === "main" ? "0" : e.slot ? String(e.slot) : "-";
	return (
		<Line>
			<Text color={selected ? "cyan" : undefined} bold={selected}>
				{selected ? "▸" : " "}
			</Text>
			<Text color="magenta">{e.current ? "*" : " "}</Text>
			<Text> </Text>
			<Text bold={selected} color={selected ? "cyan" : undefined}>
				{pad(e.name, cols.name)}
			</Text>
			<Text> </Text>
			{cols.branch ? <Text dimColor>{pad(e.branch, cols.branch) + " "}</Text> : null}
			<Text>{pad(slot, cols.slot)} </Text>
			<Text color={st.color}>{pad(st.text, cols.state)}</Text>
			<Text> </Text>
			<Text>{pad(here, cols.here)}</Text>
			<Text> </Text>
			<Text color={vs.color}>{clip(vs.text, cols.vs)}</Text>
		</Line>
	);
}

// ── The detail of the selected checkout ───────────────────────────────

/** The detail as rows, so the caller can show the ones that fit. */
export function detailLines(e: Entry, width: number, baseRef: string): React.ReactElement[] {
	const out: React.ReactElement[] = [];
	const add = (key: string, node: React.ReactNode) => out.push(<Line key={key}>{node}</Line>);
	const st = stackState(e);

	add(
		"head",
		<>
			<Text bold>{e.name}</Text>
			<Text dimColor>{e.kind === "main" ? "  main checkout" : `  ${e.branch}`}</Text>
			<Text dimColor>{e.kind === "worktree" ? (e.slot ? ` · slot ${e.slot}` : " · no slot yet") : ""}</Text>
			<Text> · </Text>
			<Text color={st.color}>{e.up
					? "stack up"
					: e.kind === "main"
						? "stack DOWN"
						: e.idleStopped
							? `stopped after ${e.idleStopped} idle`
							: "stack not running"}</Text>
			<Text dimColor>{e.memory ? ` · ${bytes(e.memory)}` : ""}</Text>
			<Text dimColor>{`  ${e.root}`}</Text>
		</>,
	);
	if (e.error) {
		add("error", <Text color="red">{`can't plan it: ${e.error}`}</Text>);
		return out;
	}
	if (e.commit) add("commit", <Text dimColor>{`${e.commit} ${e.subject}`}</Text>);

	if (e.kind === "main") {
		const parts: string[] = [];
		if (e.behind > 0) parts.push(`${e.behind} commit${e.behind === 1 ? "" : "s"} behind its upstream (U pulls them)`);
		if (e.dirty > 0) parts.push(`${e.dirty} file${e.dirty === 1 ? "" : "s"} with uncommitted changes`);
		if (parts.length) add("vs", <Text color="yellow">{parts.join(" · ")}</Text>);
	} else {
		if (e.stack.length > 1) add("stack", <Text>{`stacked: ${[baseRef, ...e.stack].join(" ← ")}`}</Text>);
		const counts: string[] = [];
		if (e.behind > 0) counts.push(`${e.behind} behind main`);
		if (e.ahead > 0) counts.push(`${e.ahead} ahead`);
		const u = e.update;
		add(
			"vs",
			<>
				<Text>{counts.length ? counts.join(", ") : "level with main"}</Text>
				{u ? (
					<Text color={u.hand ? (u.conflicts?.length ? "red" : "yellow") : "gray"}>{` · update: ${u.note}`}</Text>
				) : null}
			</>,
		);
		if (u?.conflicts?.length) {
			add("conflicts", <Text color="red">{`  conflicting: ${u.conflicts.join(", ")}`}</Text>);
		}
		const far = e.farBehind;
		if (far) {
			add(
				"far",
				<Text color={far.conflicts.length ? "red" : "yellow"}>
					{far.conflicts.length
						? `! far behind main: merging it conflicts in ${far.conflicts.join(", ")}`
						: `! far behind main: ${far.behind} commits, and the merge only gets harder`}
				</Text>,
			);
		}
	}

	const addresses = e.services.filter((s) => s.address).map((s) => `${s.name} ${s.address}`);
	if (addresses.length) add("address", <Text>{`address: ${addresses.join(", ")}`}</Text>);

	if (e.pending) {
		add("blank1", " ");
		add("pending", <Text dimColor>planning…</Text>);
		return out;
	}

	// Services.
	const { shown, hidden } = detailServices(e);
	const nameW = Math.min(24, Math.max(8, ...shown.map((s) => s.name.length + 1)));
	const runsW = 13;
	const portW = 6;
	add("blank1", " ");
	add(
		"svc-head",
		<Text dimColor>{"  " + pad("SERVICE", nameW) + " " + pad("RUNS", runsW) + " " + pad("PORT", portW) + " STATE"}</Text>,
	);
	for (const s of shown) {
		const g = healthGlyph(s.health);
		const runs = s.runs === "here" ? (s.off ? "here (off)" : "here") : s.runs === "routed" ? "main, routed" : "main";
		add(
			`svc-${s.name}`,
			<>
				<Text color={g.color}>{g.glyph} </Text>
				<Text>{pad(s.name, nameW)} </Text>
				<Text color={s.runs === "here" ? "cyan" : undefined} dimColor={s.runs !== "here"}>
					{pad(runs, runsW)}
				</Text>
				<Text> {pad(s.port ? String(s.port) : "-", portW)} </Text>
				<Text>{s.state}</Text>
				<Text dimColor>{s.memory ? `  ${bytes(s.memory)}` : ""}</Text>
				<Text dimColor>{s.runs === "here" && s.why && e.kind === "worktree" ? `  ${s.why}` : ""}</Text>
			</>,
		);
	}
	if (shown.length === 0) add("svc-none", <Text dimColor>  nothing runs here</Text>);
	if (hidden > 0) {
		add("svc-hidden", <Text dimColor>{`  (${hidden} more of main's services aren't used by anything here)`}</Text>);
	}

	// Problems.
	add("blank2", " ");
	if (e.problems.length === 0) {
		add(
			"ok",
			<Text color="green">
				{e.up || e.kind === "main"
					? "✓ nothing wrong"
					: "✓ nothing in the way (its stack isn't running: u starts it)"}
			</Text>,
		);
	}
	e.problems.forEach((p, i) => {
		const mark = p.passing ? "◐" : p.blocks ? "✗" : "!";
		const color = p.passing ? "yellow" : p.blocks ? "red" : "yellow";
		const [first, ...rest] = p.text.split("\n");
		wrap(first!, width - 2).forEach((l, j) =>
			add(
				`p${i}-a${j}`,
				<>
					<Text color={color}>{j === 0 ? `${mark} ` : "  "}</Text>
					<Text>{l}</Text>
				</>,
			),
		);
		rest.forEach((r, j) =>
			wrap(r, width - 2).forEach((l, k) => add(`p${i}-b${j}-${k}`, <Text dimColor>{"  " + l}</Text>)),
		);
		if (p.fix) {
			const lines = wrap(p.fix, width - 7 - (p.run ? 13 : 0), "");
			lines.forEach((l, j) =>
				add(
					`p${i}-fix${j}`,
					<>
						<Text dimColor>{j === 0 ? "  fix: " : "       "}</Text>
						<Text color="cyan">{l}</Text>
						<Text dimColor>{p.run && j === lines.length - 1 ? "  (f runs it)" : ""}</Text>
					</>,
				),
			);
		}
	});
	return out;
}

// ── Overlays ──────────────────────────────────────────────────────────

export function Confirm({ title, lines: raw, height, width }: { title: string; lines: string[]; height: number; width: number }) {
	const lines = raw.flatMap((l) => wrap(l, width - 2));
	const room = Math.max(0, height - 3);
	const shown = lines.length > room ? [...lines.slice(0, room - 1), `… ${lines.length - room + 1} more lines`] : lines;
	return (
		<Box flexDirection="column" height={height}>
			<Line>
				<Text bold color="yellow">
					{title}
				</Text>
			</Line>
			{shown.map((l, i) => (
				<Line key={i}>
					<Text dimColor>{"  " + l}</Text>
				</Line>
			))}
			<Line> </Line>
			<Line>
				<Text color="cyan" bold>
					y
				</Text>
				<Text dimColor> yes, do it · </Text>
				<Text color="cyan" bold>
					n
				</Text>
				<Text dimColor>/esc no</Text>
			</Line>
		</Box>
	);
}

export function Picker({ title, items, index, height }: { title: string; items: string[]; index: number; height: number }) {
	const room = Math.max(1, height - 2);
	const start = Math.max(0, Math.min(index - Math.floor(room / 2), items.length - room));
	return (
		<Box flexDirection="column" height={height}>
			<Line>
				<Text bold>{title}</Text>
				<Text dimColor> · ↑↓ choose · ⏎ ok · esc cancel</Text>
			</Line>
			{items.slice(start, start + room).map((it, i) => (
				<Line key={it + i}>
					<Text color={start + i === index ? "cyan" : undefined} bold={start + i === index}>
						{(start + i === index ? "▸ " : "  ") + it}
					</Text>
				</Line>
			))}
		</Box>
	);
}

export function Output({ title, code, text, height, width }: { title: string; code: number; text: string; height: number; width: number }) {
	const lines = wrap(text.replace(/\s+$/, ""), width - 2);
	const room = Math.max(0, height - 2);
	const shown = lines.length > room ? lines.slice(lines.length - room) : lines;
	return (
		<Box flexDirection="column" height={height}>
			<Line>
				<Text color={code === 0 ? "green" : "red"} bold>
					{code === 0 ? "✓ " : `✗ exit ${code}: `}
				</Text>
				<Text bold>{title}</Text>
				<Text dimColor> · esc closes</Text>
			</Line>
			{shown.map((l, i) => (
				<Line key={i}>{"  " + l}</Line>
			))}
		</Box>
	);
}

export const KEYS: Array<[string, string]> = [
	["↑↓ j k", "choose a checkout"],
	["⏎", "open its stack: process-compose attach"],
	["", "  in it, F10 (or ctrl-c) then Yes comes back; the stack keeps running"],
	["u", "start its stack: hydra up -D"],
	["d", "stop its stack: hydra down (asks first)"],
	["r", "restart a service: hydra process restart NAME"],
	["l", "follow a service's logs: hydra process logs NAME -f; ctrl-c ends"],
	["o", "open a service's address by name in the browser (x-hydra hosts)"],
	["f", "run the fix of one of its problems (asks first)"],
	["m", "merge main into this worktree: hydra update --here (asks first)"],
	["U", "update all: hydra update, which pulls main (dry run, then asks)"],
	["R", "read everything again now"],
	["", "  VS MAIN: ↓ commits behind main, ↑ ahead; \"on X\": stacked on branch X"],
	["", "  ! in VS MAIN: far behind main (past x-hydra behind_warn), or merging it conflicts"],
	["", "  STACK idle-off: hydra stopped it, unused for x-hydra idle_stop; u starts it"],
	["pgup pgdn", "scroll the detail"],
	["?", "this help"],
	["q", "quit; every stack keeps running"],
];

export function Help({ height }: { height: number }) {
	return (
		<Box flexDirection="column" height={height}>
			<Line>
				<Text bold>Keys</Text>
				<Text dimColor> · each runs the hydra command an agent would, in that checkout · esc closes</Text>
			</Line>
			{KEYS.slice(0, Math.max(0, height - 1)).map(([k, what], i) => (
				<Line key={i}>
					<Text color="cyan" bold>
						{pad(k, 11)}
					</Text>
					<Text>{what}</Text>
				</Line>
			))}
		</Box>
	);
}

export function Footer({ kind, width }: { kind: "main" | "worktree" | undefined; width: number }) {
	const narrow = width < 100;
	const keys: Array<[string, string]> = [
		["⏎", "open"],
		["u", "up"],
		["d", "down"],
		["r", "restart"],
		["l", "logs"],
	];
	if (!narrow || kind === "main") keys.push(["f", "fix"]);
	if (kind === "worktree") keys.push(["m", narrow ? "merge" : "merge main"]);
	keys.push(["U", narrow ? "update" : "update all"], ["?", "help"], ["q", "quit"]);
	const sep = narrow ? "  " : " · ";
	return (
		<Line>
			{keys.map(([k, what], i) => (
				<React.Fragment key={k}>
					{i > 0 ? <Text dimColor>{sep}</Text> : null}
					<Text color="cyan" bold>
						{k}
					</Text>
					<Text dimColor> {what}</Text>
				</React.Fragment>
			))}
		</Line>
	);
}

const FRAMES = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];

export function Spinner() {
	const [i, setI] = useState(0);
	useEffect(() => {
		const t = setInterval(() => setI((n) => (n + 1) % FRAMES.length), 100);
		return () => clearInterval(t);
	}, []);
	return <Text color="yellow">{FRAMES[i]}</Text>;
}
