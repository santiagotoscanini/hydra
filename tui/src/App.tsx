import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Box, Text, useApp, useInput, useStdout } from "ink";
import type { Backend, Result } from "./hydra.js";
import { onRedraw } from "./hydra.js";
import {
	detailServices,
	displayName,
	entries,
	fixCommand,
	outOfDate,
	runsHere,
	shortBranch,
	tree,
	type Entry,
	type Node,
	type Staleness,
} from "./model.js";
import type { Problem, Snapshot } from "./types.js";
import { themeFor, type Theme } from "./theme.js";
import {
	ConfirmBody,
	Footer,
	HelpBody,
	ListRow,
	OutputBody,
	Panel,
	PickerBody,
	Row,
	ThemeContext,
	detailRows,
	detailTitle,
	problemRows,
	problemsTitle,
	type Seg,
} from "./ui.js";

type Overlay =
	| { kind: "help" }
	| { kind: "confirm"; title: string; lines: string[]; onYes: () => void }
	| { kind: "pick"; title: string; items: string[]; index: number; onPick: (i: number) => void }
	| { kind: "output"; title: string; code: number; text: string };

/** An action the dashboard runs in a checkout: "starting", "stopping". */
interface Busy {
	verb: string;
	what: string;
}

interface Props {
	backend: Backend;
	/** Fixed terminal size, for tests; otherwise the real one. */
	size?: { columns: number; rows: number };
	/** A snapshot to start from (tests). */
	initial?: Snapshot;
	/** The terminal's light or dark theme (theme.ts); unknown when left out. */
	theme?: Theme;
	home?: string;
}

function useSize(fixed?: { columns: number; rows: number }) {
	const { stdout } = useStdout();
	const read = () => fixed ?? { columns: stdout.columns || 80, rows: stdout.rows || 24 };
	const [size, setSize] = useState(read);
	useEffect(() => {
		if (fixed) return;
		const on = () => setSize(read());
		stdout.on("resize", on);
		return () => {
			stdout.off("resize", on);
		};
	}, [stdout, fixed]);
	return size;
}

/** What a checkout is called in a message: its ticket id, or its branch. */
function called(e: Entry): string {
	const n = displayName(e);
	return n.id ?? n.title;
}

export default function App({ backend, size: fixedSize, initial, theme = themeFor(), home }: Props) {
	const { exit } = useApp();
	const size = useSize(fixedSize);
	const [snap, setSnap] = useState<Snapshot | undefined>(initial);
	const [error, setError] = useState<string>();
	const [selRoot, setSelRoot] = useState<string>();
	const [overlay, setOverlay] = useState<Overlay>();
	const [busy, setBusy] = useState<Record<string, Busy>>({});
	const [message, setMessage] = useState<{ text: string; color?: string }>();
	const [reading, setReading] = useState(false);
	const [scroll, setScroll] = useState(0);
	const [blank, setBlank] = useState(false);
	const preselected = useRef(false);

	useEffect(() => {
		// After a command had the terminal, the screen is empty but Ink thinks
		// its last frame is still there, and skips drawing the same one again.
		// One empty frame, then the real one, makes it draw everything.
		onRedraw(() => {
			setBlank(true);
			setTimeout(() => setBlank(false), 80);
		});
		return backend.watch(
			(s) => {
				setSnap(s);
				setError(undefined);
				setReading(false);
			},
			(msg) => setError(msg),
		);
	}, [backend]);

	// In the order drawn, so ↑ and ↓ move down the tree as it reads.
	const nodes = useMemo(() => (snap ? tree(entries(snap)) : []), [snap]);
	const list = useMemo(() => nodes.map((n) => n.e), [nodes]);
	const nodeOf = useMemo(() => new Map<Entry, Node>(nodes.map((n) => [n.e, n])), [nodes]);
	const staleOf = (e: Entry): Staleness => nodeOf.get(e)?.stale ?? { kind: "none" };
	// The checkout hydra was started in, until someone moves.
	useEffect(() => {
		if (preselected.current || list.length === 0) return;
		preselected.current = true;
		setSelRoot((list.find((e) => e.current) ?? list[0]!).root);
	}, [list]);
	let index = list.findIndex((e) => e.root === selRoot);
	if (index < 0) index = 0;
	const sel: Entry | undefined = list[index];
	const mainRoot = snap?.main?.root ?? snap?.repo ?? "";

	const select = (i: number) => {
		const e = list[Math.max(0, Math.min(list.length - 1, i))];
		if (e) {
			setSelRoot(e.root);
			setScroll(0);
		}
	};

	// A success says so for a while, then makes room.
	useEffect(() => {
		if (message?.color !== "green") return;
		const t = setTimeout(() => setMessage((m) => (m === message ? undefined : m)), 8000);
		return () => clearTimeout(t);
	}, [message]);

	/** Runs hydra in the background in a checkout, shows it under way in
	 *  its row and panel, and says how it went. */
	const runQuiet = useCallback(
		(args: string[], cwd: string, name: string, verb: string, after?: (r: Result) => void) => {
			const now = busy[cwd];
			if (now) {
				setMessage({ text: `wait: ${now.what} is still running`, color: "yellow" });
				return;
			}
			const what = `hydra ${args.join(" ")}` + (name ? ` in ${name}` : "");
			setBusy((b) => ({ ...b, [cwd]: { verb, what } }));
			setMessage(undefined);
			void backend.run(args, cwd).then((r) => {
				setBusy((b) => {
					const { [cwd]: _, ...rest } = b;
					return rest;
				});
				backend.refresh(cwd === mainRoot ? undefined : cwd);
				if (after) return after(r);
				const last = r.output.trim().split("\n").filter(Boolean).pop() ?? "";
				if (r.code === 0) setMessage({ text: `✓ ${what}${last ? `: ${last}` : ""}`, color: "green" });
				else {
					setMessage({ text: `✗ ${what} failed (exit ${r.code})`, color: "red" });
					setOverlay({ kind: "output", title: what, code: r.code, text: r.output });
				}
			});
		},
		[backend, busy, mainRoot],
	);

	const inTerminal = (cmd: Parameters<Backend["terminal"]>[0], refreshRoot?: string) => {
		setOverlay(undefined);
		const code = backend.terminal(cmd);
		backend.refresh(refreshRoot);
		return code;
	};

	/** A dry run first, then a confirmation with what it printed. */
	const dryThenDo = (dryArgs: string[], realArgs: string[], cwd: string, title: string, refreshRoot?: string) => {
		runQuiet(dryArgs, cwd, "", "checking", (r) => {
			const lines = r.output.replace(/\s+$/, "").split("\n");
			if (r.code !== 0 && r.code !== 1) {
				setOverlay({ kind: "output", title: `hydra ${dryArgs.join(" ")}`, code: r.code, text: r.output });
				return;
			}
			setOverlay({
				kind: "confirm",
				title,
				lines,
				onYes: () =>
					inTerminal(
						{ argv: ["hydra", ...realArgs], cwd, title: `hydra ${realArgs.join(" ")}`, pause: true },
						refreshRoot,
					),
			});
		});
	};

	const pickService = (title: string, names: string[], then: (name: string) => void) => {
		if (names.length === 0) {
			setMessage({ text: "no service to choose from here", color: "yellow" });
			return;
		}
		if (names.length === 1) return then(names[0]!);
		setOverlay({ kind: "pick", title, items: names, index: 0, onPick: (i) => then(names[i]!) });
	};

	const act = (key: string, e: Entry) => {
		switch (key) {
			case "open":
				if (!e.up) {
					setMessage({ text: `${called(e)}'s stack isn't running: u starts it`, color: "yellow" });
					return;
				}
				inTerminal(
					{
						argv: ["hydra", "attach"],
						cwd: e.root,
						title: "hydra attach  (F10 or ctrl-c, then Yes: back to the dashboard; the stack keeps running)",
						pause: "failed",
					},
					e.root,
				);
				return;
			case "up":
				runQuiet(["up", "-D"], e.root, called(e), "starting");
				return;
			case "down":
				if (!e.up) {
					setMessage({ text: `${called(e)}'s stack isn't running`, color: "yellow" });
					return;
				}
				setOverlay({
					kind: "confirm",
					title: `Stop ${called(e)}'s stack? (hydra down)`,
					lines:
						e.kind === "main"
							? ["Every worktree that takes a service from main loses it until main is up again."]
							: [`It runs: ${runsHere(e).map((s) => s.name).join(", ") || "nothing"}.`],
					onYes: () => {
						setOverlay(undefined);
						runQuiet(["down"], e.root, called(e), "stopping");
					},
				});
				return;
			case "restart": {
				const names = (e.kind === "main" ? e.services.filter((s) => s.health !== "off") : runsHere(e)).map(
					(s) => s.name,
				);
				if (!e.up) {
					setMessage({ text: `${called(e)}'s stack isn't running: u starts it`, color: "yellow" });
					return;
				}
				pickService(`Restart which of ${called(e)}'s services?`, names, (n) => {
					setOverlay(undefined);
					runQuiet(["process", "restart", n], e.root, called(e), `restarting ${n}`);
				});
				return;
			}
			case "logs": {
				const svcs = detailServices(e).shown;
				pickService(
					"Follow whose logs?",
					svcs.map((s) => (s.runs === "here" ? s.name : `${s.name} (main's)`)),
					(label) => {
						const name = label.replace(/ \(main's\)$/, "");
						const cwd = label.endsWith("(main's)") ? mainRoot : e.root;
						inTerminal({
							argv: ["hydra", "process", "logs", name, "-f", "-n", "300"],
							cwd,
							title: `hydra process logs ${name} -f   (ctrl-c: back to the dashboard)`,
							pause: "failed",
						});
					},
				);
				return;
			}
			case "browse": {
				const withAddress = e.services.filter((s) => s.address);
				if (withAddress.length === 0) {
					setMessage({ text: "no service has an address by name (x-hydra hosts)", color: "yellow" });
					return;
				}
				pickService(
					"Open whose address?",
					withAddress.map((s) => `${s.name} ${s.address}`),
					(label) => {
						setOverlay(undefined);
						const url = label.slice(label.indexOf(" ") + 1);
						if (!e.up) setMessage({ text: `${called(e)}'s stack isn't running, so ${url} won't answer: u starts it`, color: "yellow" });
						void backend.browse(url).then((r) => {
							if (r.code !== 0) setMessage({ text: r.output || `couldn't open ${url}`, color: "red" });
							else if (e.up) setMessage({ text: `opened ${url}` });
						});
					},
				);
				return;
			}
			case "fix": {
				const fixes = e.problems.filter((p): p is Problem & { run: string } => !!p.run);
				if (fixes.length === 0) {
					setMessage({ text: "no fix to run here", color: "yellow" });
					return;
				}
				const confirmFix = (p: Problem & { run: string }) =>
					setOverlay({
						kind: "confirm",
						title: `Run in ${called(e)}: ${p.run}`,
						lines: p.text.split("\n"),
						onYes: () =>
							inTerminal(
								{ shell: fixCommand(p.run, backend.exe), cwd: e.root, title: p.run, pause: true },
								undefined,
							),
					});
				if (fixes.length === 1) return confirmFix(fixes[0]!);
				setOverlay({
					kind: "pick",
					title: "Run which fix?",
					items: fixes.map((p) => p.run),
					index: 0,
					onPick: (i) => confirmFix(fixes[i]!),
				});
				return;
			}
			case "merge": {
				if (e.kind === "main") {
					setMessage({ text: "m merges main into a worktree; U updates main itself", color: "yellow" });
					return;
				}
				// In step with its parent: merging it changes nothing until
				// the branch above that is behind is brought up to date.
				const s = staleOf(e);
				if (s.kind === "inherits" && !e.update?.action) {
					setMessage({
						text: `${called(e)} has everything ${shortBranch(e.parent ?? "main")} has; ${shortBranch(s.from.branch)} is behind: update it first (select it, m), or U updates all, parents first`,
						color: "yellow",
					});
					return;
				}
				dryThenDo(
					["update", "--here", "--dry-run"],
					["update", "--here"],
					e.root,
					`Merge ${e.parent && e.stack.length > 1 ? shortBranch(e.parent) : "main"} into ${called(e)}? (hydra update --here)`,
					e.root,
				);
				return;
			}
			case "update":
				dryThenDo(
					["update", "--dry-run"],
					["update"],
					mainRoot,
					"Update everything? (hydra update: pulls main, restarts what moved, merges main into the worktrees)",
				);
				return;
		}
	};

	useInput((input, key) => {
		// Keys typed faster than a frame arrive together ("jjj").
		if (input.length > 1 && /^[jk]+$/.test(input) && !overlay) {
			const n = Array.from(input).reduce((d, c) => d + (c === "j" ? 1 : -1), 0);
			return select(index + n);
		}
		if (input.length > 1 && !key.ctrl && !key.meta) input = input[0]!;
		if (key.ctrl && input === "c") {
			exit();
			return;
		}
		if (overlay) {
			switch (overlay.kind) {
				case "confirm":
					if (input === "y" || input === "Y") overlay.onYes();
					else if (input === "n" || input === "N" || key.escape || input === "q") setOverlay(undefined);
					return;
				case "pick":
					if (key.upArrow || input === "k")
						setOverlay({ ...overlay, index: Math.max(0, overlay.index - 1) });
					else if (key.downArrow || input === "j")
						setOverlay({ ...overlay, index: Math.min(overlay.items.length - 1, overlay.index + 1) });
					else if (key.return) overlay.onPick(overlay.index);
					else if (key.escape || input === "q") setOverlay(undefined);
					return;
				default:
					if (key.escape || input === "q" || key.return || input === "?") {
						setOverlay(undefined);
						return;
					}
			}
		}
		if (input === "q") {
			exit();
			return;
		}
		if (key.upArrow || input === "k") return select(index - 1);
		if (key.downArrow || input === "j") return select(index + 1);
		if (key.pageDown) return setScroll((s) => s + 5);
		if (key.pageUp) return setScroll((s) => Math.max(0, s - 5));
		if (input === "?") return setOverlay({ kind: "help" });
		if (input === "R") {
			backend.refresh();
			setReading(true);
			return;
		}
		if (key.escape) return setMessage(undefined);
		if (!sel) return;
		const keys: Record<string, string> = {
			u: "up",
			d: "down",
			r: "restart",
			l: "logs",
			o: "browse",
			f: "fix",
			m: "merge",
			U: "update",
		};
		if (key.return) return act("open", sel);
		if (keys[input]) act(keys[input]!, sel);
	});

	// ── Layout ──
	// One row short of the terminal: a frame as tall as the screen makes Ink
	// clear the whole terminal on every change, which flickers.
	const width = size.columns;
	const rows = Math.max(10, size.rows - 1);
	const busyList = Object.values(busy);
	let status: Seg[] = [];
	if (error) status = [{ text: "✗ " + error, color: "red" }];
	else if (snap?.error) status = [{ text: "✗ " + snap.error, color: "red" }];
	else if (busyList.length)
		status = [{ text: "", spin: true, color: "yellow" }, { text: " " + busyList.map((b) => b.what).join(" · ") + "…" }];
	else if (message) status = [{ text: message.text, color: message.color }];
	// The header, the footer, and a status line only while there is news.
	const body = rows - 2 - (status.length ? 1 : 0);
	// Wide: the list and the selection's problems on the left, its detail on
	// the right. Narrow: the list above the detail, which leads with the
	// problems.
	const wide = width >= 120;
	const listW = wide ? Math.min(84, Math.max(60, Math.floor(width * 0.42))) : width;
	const listH = wide
		? Math.min(list.length + 2, Math.max(5, Math.floor(body * 0.6)))
		: Math.min(list.length + 2, Math.max(5, Math.floor(body * 0.4)));
	const detailW = wide ? width - listW : width;
	const detailH = wide ? body : body - listH;
	const listInner = listW - 2;
	const detailInner = Math.max(10, detailW - 4); // borders and a column of padding each side
	const detailRoom = Math.max(1, detailH - 2);
	const visible = Math.max(1, listH - 2);
	const start = Math.max(0, Math.min(index - Math.floor(visible / 2), list.length - visible));
	const verbOf = (e: Entry) => busy[e.root]?.verb;

	const wtUp = snap ? snap.worktrees.filter((w) => w.up).length : 0;
	const stale = nodes.filter(outOfDate).length;
	const anyPending = !!snap?.worktrees.some((w) => w.pending);

	// The detail panel, or an overlay in its place.
	let title: Seg[];
	let color = "gray";
	let content: React.ReactNode;
	if (overlay?.kind === "help") {
		title = [{ text: "Keys", bold: true }];
		color = "cyan";
		content = <HelpBody width={detailInner} height={detailRoom} />;
	} else if (overlay?.kind === "confirm") {
		title = [{ text: overlay.title, bold: true }];
		color = "yellow";
		content = <ConfirmBody lines={overlay.lines} width={detailInner} height={detailRoom} />;
	} else if (overlay?.kind === "pick") {
		title = [{ text: overlay.title, bold: true }];
		color = "cyan";
		content = <PickerBody items={overlay.items} index={overlay.index} width={detailInner} height={detailRoom} />;
	} else if (overlay?.kind === "output") {
		title = [{ text: overlay.code === 0 ? "✓ " : `✗ exit ${overlay.code}: `, bold: true }, { text: overlay.title, bold: true }];
		color = overlay.code === 0 ? "green" : "red";
		content = <OutputBody text={overlay.text} width={detailInner} height={detailRoom} />;
	} else if (sel) {
		const lines = detailRows(sel, detailInner, { home, busy: verbOf(sel), problems: !wide, node: nodeOf.get(sel), staleOf });
		const max = Math.max(0, lines.length - detailRoom);
		const from = Math.min(scroll, max);
		const shown = lines.slice(from, from + detailRoom);
		if (lines.length > from + detailRoom && shown.length > 0) {
			shown[shown.length - 1] = [{ text: `… ${lines.length - from - detailRoom + 1} more lines (pgdn)`, dim: true }];
		}
		title = detailTitle(sel, verbOf(sel));
		content = shown.map((segs, i) => <Row key={i} segs={segs} width={detailInner} />);
	} else {
		title = [];
		content = null;
	}

	const listPanel = (
		<Panel
			title={[{ text: "Checkouts", bold: true, color: undefined }, { text: snap ? ` ${list.length}` : "" }]}
			color="cyan"
			width={listW}
			height={listH}
		>
			{snap ? (
				nodes
					.slice(start, start + visible)
					.map((n) => (
						<ListRow key={n.e.root} node={n} selected={n.e.root === sel?.root} width={listInner} busy={verbOf(n.e)} />
					))
			) : (
				<Row width={listInner} segs={error ? [] : [{ text: " " }, { text: "", spin: true, color: "cyan" }, { text: " reading the repository…" }]} />
			)}
		</Panel>
	);
	const detailPanel = (
		<Panel title={title} color={color} width={detailW} height={detailH}>
			<Box flexDirection="column" paddingX={1}>
				{content}
			</Box>
		</Panel>
	);

	// Wide: the selection's problems under the list.
	let problemsPanel: React.ReactNode = null;
	if (wide) {
		const h = body - listH;
		const inner = listW - 4;
		const pt = sel ? problemsTitle(sel) : { title: [], color: "gray" };
		const rowsP = sel ? problemRows(sel, inner) : [];
		const room = Math.max(1, h - 2);
		const shownP = rowsP.slice(0, room);
		if (rowsP.length > room) shownP[room - 1] = [{ text: `… ${rowsP.length - room + 1} more lines`, dim: true }];
		problemsPanel = (
			<Panel title={pt.title} color={pt.color} width={listW} height={h}>
				<Box flexDirection="column" paddingX={1}>
					{shownP.map((segs, i) => (
						<Row key={i} segs={segs} width={inner} />
					))}
				</Box>
			</Panel>
		);
	}

	/** The top line: the repository, main, and how many worktrees are up
	 *  and out of date. Main behind its upstream is said here once: every
	 *  branch is behind then, and the rows don't repeat it. */
	function header(w: number): Seg[] {
		const n = snap?.worktrees.length ?? 0;
		const main: Seg[] = snap?.main
			? [
					{ text: "   main " },
					{ text: snap.main.up ? "up" : "down", color: snap.main.up ? "green" : "red", bold: true },
					...(snap.main.behind > 0 ? [{ text: `, ${snap.main.behind} behind upstream (U)`, color: "yellow" }] : []),
					{ text: ` · ${wtUp} of ${n} worktree${n === 1 ? "" : "s"} up` },
					...(n
						? [{ text: " · " }, stale ? { text: `${stale} out of date`, color: "yellow", bold: true } : { text: "all in step", dim: true }]
						: []),
				]
			: [];
		const busyNote: Seg[] =
			reading || anyPending
				? [{ text: "   " }, { text: "", spin: true, color: "cyan" }, { text: reading ? " reading everything again" : " planning" }]
				: [];
		const repo: Seg[] = [{ text: snap ? "  " + snap.repo.slice(snap.repo.lastIndexOf("/") + 1) : "", bold: true }];
		const segs = (withRepo: boolean): Seg[] => [{ text: " hydra", bold: true, color: "magenta" }, ...(withRepo ? repo : []), ...main, ...busyNote];
		const width_ = (x: Seg[]) => x.reduce((k, s) => k + (s.spin ? 1 : Array.from(s.text).length), 0);
		// Narrow: the repository's name goes first, it is the least news.
		return width_(segs(true)) <= w ? segs(true) : segs(false);
	}

	if (blank) return <Box />;
	return (
		<ThemeContext.Provider value={theme}>
			<Box flexDirection="column" width={width} height={rows}>
				<Row width={width} segs={header(width)} />
				{overlay?.kind === "help" ? (
					<Panel title={[{ text: "Keys", bold: true, color: undefined }]} color="cyan" width={width} height={body}>
						<Box flexDirection="column" paddingX={1}>
							<HelpBody width={width - 4} height={body - 2} />
						</Box>
					</Panel>
				) : wide ? (
					<Box flexDirection="row" height={body}>
						<Box flexDirection="column" width={listW} height={body}>
							{listPanel}
							{problemsPanel}
						</Box>
						{detailPanel}
					</Box>
				) : (
					<>
						{listPanel}
						{detailPanel}
					</>
				)}
				{status.length ? <Row width={width} segs={[{ text: " " }, ...status]} /> : null}
				<Footer e={sel} width={width} overlay={!!overlay} />
			</Box>
		</ThemeContext.Provider>
	);
}
