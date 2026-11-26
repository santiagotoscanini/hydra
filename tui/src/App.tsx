import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Box, Text, useApp, useInput, useStdout } from "ink";
import type { Backend, Result } from "./hydra.js";
import { onRedraw } from "./hydra.js";
import { detailServices, entries, fixCommand, runsHere, type Entry } from "./model.js";
import type { Problem, Snapshot } from "./types.js";
import {
	Confirm,
	Footer,
	Help,
	Line,
	ListHeader,
	ListRow,
	Output,
	Picker,
	Spinner,
	columns,
	detailLines,
} from "./ui.js";

type Overlay =
	| { kind: "help" }
	| { kind: "confirm"; title: string; lines: string[]; onYes: () => void }
	| { kind: "pick"; title: string; items: string[]; index: number; onPick: (i: number) => void }
	| { kind: "output"; title: string; code: number; text: string };

interface Busy {
	what: string;
}

interface Props {
	backend: Backend;
	/** Fixed terminal size, for tests; otherwise the real one. */
	size?: { columns: number; rows: number };
	/** A snapshot to start from (tests). */
	initial?: Snapshot;
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

function ago(at: string, now: number): string {
	const s = Math.max(0, Math.round((now - Date.parse(at)) / 1000));
	return s < 2 ? "now" : `${s}s ago`;
}

export default function App({ backend, size: fixedSize, initial }: Props) {
	const { exit } = useApp();
	const size = useSize(fixedSize);
	const [snap, setSnap] = useState<Snapshot | undefined>(initial);
	const [error, setError] = useState<string>();
	const [selRoot, setSelRoot] = useState<string>();
	const [overlay, setOverlay] = useState<Overlay>();
	const [busy, setBusy] = useState<Busy>();
	const [message, setMessage] = useState<{ text: string; color?: string }>();
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
			},
			(msg) => setError(msg),
		);
	}, [backend]);

	const list = useMemo(() => (snap ? entries(snap) : []), [snap]);
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

	/** Runs hydra in the background and says how it went. */
	const runQuiet = useCallback(
		(args: string[], cwd: string, name: string, after?: (r: Result) => void) => {
			if (busy) {
				setMessage({ text: `wait: ${busy.what} is still running`, color: "yellow" });
				return;
			}
			const what = `hydra ${args.join(" ")}` + (name ? ` in ${name}` : "");
			setBusy({ what });
			setMessage(undefined);
			void backend.run(args, cwd).then((r) => {
				setBusy(undefined);
				backend.refresh(cwd === mainRoot ? undefined : cwd);
				if (after) return after(r);
				const last = r.output.trim().split("\n").filter(Boolean).pop() ?? "";
				if (r.code === 0) setMessage({ text: `✓ ${what}${last ? `: ${last}` : ""}`, color: "green" });
				else setOverlay({ kind: "output", title: what, code: r.code, text: r.output });
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
		runQuiet(dryArgs, cwd, "", (r) => {
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
					setMessage({ text: `${e.name}'s stack isn't running: u starts it`, color: "yellow" });
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
				runQuiet(["up", "-D"], e.root, e.name);
				return;
			case "down":
				if (!e.up) {
					setMessage({ text: `${e.name}'s stack isn't running`, color: "yellow" });
					return;
				}
				setOverlay({
					kind: "confirm",
					title: `Stop ${e.kind === "main" ? "main's" : `${e.name}'s`} stack? (hydra down)`,
					lines:
						e.kind === "main"
							? ["Every worktree that takes a service from main loses it until main is up again."]
							: [`It runs: ${runsHere(e).map((s) => s.name).join(", ") || "nothing"}.`],
					onYes: () => {
						setOverlay(undefined);
						runQuiet(["down"], e.root, e.name);
					},
				});
				return;
			case "restart": {
				const names = (e.kind === "main" ? e.services.filter((s) => s.health !== "off") : runsHere(e)).map(
					(s) => s.name,
				);
				if (!e.up) {
					setMessage({ text: `${e.name}'s stack isn't running: u starts it`, color: "yellow" });
					return;
				}
				pickService(`Restart which of ${e.name}'s services?`, names, (n) => {
					setOverlay(undefined);
					runQuiet(["process", "restart", n], e.root, e.name);
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
						if (!e.up) setMessage({ text: `${e.name}'s stack isn't running, so ${url} won't answer: u starts it`, color: "yellow" });
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
						title: `Run in ${e.name}: ${p.run}`,
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
			case "merge":
				if (e.kind === "main") {
					setMessage({ text: "m merges main into a worktree; U updates main itself", color: "yellow" });
					return;
				}
				dryThenDo(
					["update", "--here", "--dry-run"],
					["update", "--here"],
					e.root,
					`Merge ${e.update?.parent ?? "main"} into ${e.name}? (hydra update --here)`,
					e.root,
				);
				return;
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
			setMessage({ text: "reading everything again…" });
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
	const rows = Math.max(8, size.rows - 1);
	const cols = columns(width);
	const listRoom = Math.max(3, Math.min(list.length + 1, Math.floor((rows - 4) * 0.45)));
	const visible = listRoom - 1;
	const start = Math.max(0, Math.min(index - Math.floor(visible / 2), list.length - visible));
	const detailRoom = Math.max(0, rows - 1 - listRoom - 1 - 1 - 1);

	const mainUp = snap?.main?.up;
	const wtUp = snap ? snap.worktrees.filter((w) => w.up).length : 0;

	let bottom: React.ReactNode;
	if (overlay?.kind === "help") bottom = <Help height={detailRoom} />;
	else if (overlay?.kind === "confirm")
		bottom = <Confirm title={overlay.title} lines={overlay.lines} height={detailRoom} width={width} />;
	else if (overlay?.kind === "pick")
		bottom = <Picker title={overlay.title} items={overlay.items} index={overlay.index} height={detailRoom} />;
	else if (overlay?.kind === "output")
		bottom = <Output title={overlay.title} code={overlay.code} text={overlay.text} height={detailRoom} width={width} />;
	else if (sel) {
		const lines = detailLines(sel, width, snap?.baseRef || "main");
		const max = Math.max(0, lines.length - detailRoom);
		const from = Math.min(scroll, max);
		const shown = lines.slice(from, from + detailRoom);
		if (lines.length > from + detailRoom && shown.length > 0) {
			shown[shown.length - 1] = (
				<Line key="more">
					<Text dimColor>{`… ${lines.length - from - detailRoom + 1} more lines (pgdn)`}</Text>
				</Line>
			);
		}
		bottom = (
			<Box flexDirection="column" height={detailRoom}>
				{shown}
			</Box>
		);
	} else {
		bottom = (
			<Box height={detailRoom}>
				<Text dimColor>{error ? "" : "reading the repository…"}</Text>
			</Box>
		);
	}

	if (blank) return <Box />;
	return (
		<Box flexDirection="column" width={width} height={rows}>
			<Line>
				<Text bold color="magenta">
					hydra
				</Text>
				{snap?.main ? (
					<>
						<Text>{"  main "}</Text>
						<Text color={mainUp ? "green" : "red"}>{mainUp ? "up" : "DOWN"}</Text>
						<Text>{` · ${wtUp}/${snap.worktrees.length} worktree${snap.worktrees.length === 1 ? "" : "s"} up`}</Text>
						<Text dimColor>{` · read ${ago(snap.at, Date.now())}`}</Text>
					</>
				) : null}
				<Text dimColor>{snap ? `  ${snap.repo}` : ""}</Text>
			</Line>
			<Box flexDirection="column" height={listRoom}>
				<ListHeader cols={cols} />
				{list.slice(start, start + visible).map((e) => (
					<ListRow key={e.root} e={e} selected={e.root === sel?.root} cols={cols} />
				))}
			</Box>
			<Line>
				<Text dimColor>{"─".repeat(Math.max(0, width))}</Text>
			</Line>
			{bottom}
			<Line>
				{busy ? (
					<>
						<Spinner />
						<Text>{` ${busy.what}…`}</Text>
					</>
				) : error ? (
					<Text color="red">{error}</Text>
				) : snap?.error ? (
					<Text color="red">{snap.error}</Text>
				) : message ? (
					<Text color={message.color}>{message.text}</Text>
				) : (
					<Text> </Text>
				)}
			</Line>
			<Footer kind={sel?.kind} width={width} />
		</Box>
	);
}
