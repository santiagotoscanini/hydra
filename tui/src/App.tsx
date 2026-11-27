import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Box, Text, useApp, useInput, useStdout, type Key } from "ink";
import type { MouseEvent, MouseSource } from "./mouse.js";
import type { Backend, Result } from "./hydra.js";
import { onRedraw } from "./hydra.js";
import {
	detailServices,
	displayName,
	donePRCount,
	entries,
	fixCommand,
	outOfDate,
	runsHere,
	labelBeside,
	labels,
	prWidth,
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
	footerSegs,
	linkAt,
	outputLines,
	pickerStart,
	segAt,
	KEYS,
	HelpBody,
	ListRow,
	OutputBody,
	rowColumns,
	Panel,
	PickerBody,
	Row,
	ThemeContext,
	detailRows,
	detailTitle,
	problemRows,
	problemsTitle,
	type Seg,
	type TunnelHeld,
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
	/** clicks, the wheel and drags (mouse.ts); none in a test that leaves it out */
	mouse?: MouseSource;
}

/** A panel's place on the screen, in cells from the top left. */
interface Area {
	x: number;
	y: number;
	w: number;
	h: number;
}
const inside = (a: Area | undefined, x: number, y: number) => !!a && x >= a.x && x < a.x + a.w && y >= a.y && y < a.y + a.h;
const clamp = (n: number, lo: number, hi: number) => Math.max(lo, Math.min(hi, n));

/** Where everything was drawn, for the mouse. */
interface Geometry {
	wide: boolean;
	list: Area;
	detail: Area;
	problems?: Area;
	/** the whole body, under the header: the help covers it */
	body: Area;
	footerY: number;
	footer: Seg[];
	/** the list's first row shown, and how many */
	start: number;
	visible: number;
	/** the detail's rows as shown, and how far it can scroll */
	detailRows: Seg[][];
	detailInner: number;
	detailMax: number;
	problemRows: Seg[][];
	problemsMax: number;
	helpMax: number;
	outputMax: number;
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


export default function App({ backend, size: fixedSize, initial, theme = themeFor(), home, mouse }: Props) {
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
	// Scrolled by the wheel: the problems' panel (wide), the help, and an
	// output (lines up from its end).
	const [pscroll, setPscroll] = useState(0);
	const [helpFrom, setHelpFrom] = useState(0);
	const [outBack, setOutBack] = useState(0);
	// Where a dragged border left the panels: the list's width (wide) and
	// its height. Until then, the defaults.
	const [listWSet, setListWSet] = useState<number>();
	const [listHSet, setListHSet] = useState<number>();
	const geo = useRef<Geometry>(undefined);
	const dragging = useRef<"width" | "height">(undefined);
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
	// What a checkout is called in a message: its ticket id, or its branch;
	// branches sharing a ticket (a PR stack) by what tells them apart.
	const names = useMemo(() => labels(list.filter((e) => e.kind !== "main" && e.branch !== "(detached)").map((e) => e.branch)), [list]);
	const called = (e: Entry): string => {
		const n = displayName(e);
		return (e.kind === "main" ? undefined : names.get(e.branch)) ?? n.id ?? n.title;
	};
	// The checkout hydra was started in, until someone moves.
	useEffect(() => {
		if (preselected.current || list.length === 0) return;
		preselected.current = true;
		setSelRoot((list.find((e) => e.current) ?? list[0]!).root);
	}, [list]);
	// Until someone moves, the checkout hydra was started in, from the first
	// frame on.
	let index = list.findIndex((e) => e.root === selRoot);
	if (index < 0 && !preselected.current) index = list.findIndex((e) => e.current);
	if (index < 0) index = 0;
	const sel: Entry | undefined = list[index];
	const mainRoot = snap?.main?.root ?? snap?.repo ?? "";
	// Each tunnel with what its holder is called here.
	const tunnelsHeld: TunnelHeld[] = (snap?.tunnels ?? []).map((t) => {
		const h = t.holder ? list.find((e) => e.root === t.holder) : undefined;
		return { t, holder: !t.holder ? "main" : h ? called(h) : t.holder.slice(t.holder.lastIndexOf("/") + 1) };
	});

	const select = (i: number) => {
		const e = list[Math.max(0, Math.min(list.length - 1, i))];
		if (e) {
			setSelRoot(e.root);
			setScroll(0);
			setPscroll(0);
		}
	};
	/** An overlay, from its top. */
	const openOverlay = (o: Overlay | undefined) => {
		setHelpFrom(0);
		setOutBack(0);
		setOverlay(o);
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
		(args: string[], cwd: string, name: string, verb: string, after?: (r: Result) => void, key = cwd) => {
			const now = busy[key];
			if (now) {
				setMessage({ text: `wait: ${now.what} is still running`, color: "yellow" });
				return;
			}
			const what = `hydra ${args.join(" ")}` + (name ? ` in ${name}` : "");
			setBusy((b) => ({ ...b, [key]: { verb, what } }));
			setMessage(undefined);
			void backend.run(args, cwd).then((r) => {
				setBusy((b) => {
					const { [key]: _, ...rest } = b;
					return rest;
				});
				backend.refresh(cwd === mainRoot ? undefined : cwd);
				if (after) return after(r);
				const last = r.output.trim().split("\n").filter(Boolean).pop() ?? "";
				if (r.code === 0) setMessage({ text: `✓ ${what}${last ? `: ${last}` : ""}`, color: "green" });
				else {
					setMessage({ text: `✗ ${what} failed (exit ${r.code})`, color: "red" });
					openOverlay({ kind: "output", title: what, code: r.code, text: r.output });
				}
			});
		},
		[backend, busy, mainRoot],
	);

	const inTerminal = (cmd: Parameters<Backend["terminal"]>[0], refreshRoot?: string) => {
		openOverlay(undefined);
		const code = backend.terminal(cmd);
		backend.refresh(refreshRoot);
		return code;
	};

	/** A dry run first, then a confirmation with what it printed. key: the
	 *  row shown busy meanwhile, when it isn't the checkout at cwd. */
	const dryThenDo = (dryArgs: string[], realArgs: string[], cwd: string, title: string, refreshRoot?: string, key = cwd) => {
		runQuiet(dryArgs, cwd, "", "checking", (r) => {
			const lines = r.output.replace(/\s+$/, "").split("\n");
			if (r.code !== 0 && r.code !== 1) {
				openOverlay({ kind: "output", title: `hydra ${dryArgs.join(" ")}`, code: r.code, text: r.output });
				return;
			}
			openOverlay({
				kind: "confirm",
				title,
				lines,
				onYes: () =>
					inTerminal(
						{ argv: ["hydra", ...realArgs], cwd, title: `hydra ${realArgs.join(" ")}`, pause: true },
						refreshRoot,
					),
			});
		}, key);
	};

	const pickService = (title: string, names: string[], then: (name: string) => void) => {
		if (names.length === 0) {
			setMessage({ text: "no service to choose from here", color: "yellow" });
			return;
		}
		if (names.length === 1) return then(names[0]!);
		openOverlay({ kind: "pick", title, items: names, index: 0, onPick: (i) => then(names[i]!) });
	};

	const act = (key: string, e: Entry) => {
		// A branch with no checkout: nothing runs there, so only m, U and p
		// do anything.
		if (e.kind === "branch" && key !== "merge" && key !== "update" && key !== "pr") {
			setMessage({ text: `${called(e)} is a branch only, with no worktree: nothing runs it. m merges its parent into it; the worktrees stacked on it run its code`, color: "yellow" });
			return;
		}
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
				openOverlay({
					kind: "confirm",
					title: `Stop ${called(e)}'s stack? (hydra down)`,
					lines:
						e.kind === "main"
							? ["Every worktree that takes a service from main loses it until main is up again."]
							: [`It runs: ${runsHere(e).map((s) => s.name).join(", ") || "nothing"}.`],
					onYes: () => {
						openOverlay(undefined);
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
					openOverlay(undefined);
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
						openOverlay(undefined);
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
			case "pr": {
				const url = e.pr?.url;
				if (!url) {
					const why = snap?.prs?.error ? ` (${snap.prs.error})` : "";
					setMessage({ text: e.kind === "main" ? "main has no PR" : `no PR known for ${called(e)}${why}`, color: "yellow" });
					return;
				}
				void backend.browse(url).then((r) => setMessage(r.code === 0 ? { text: `opened ${url}` } : { text: r.output || `couldn't open ${url}`, color: "red" }));
				return;
			}
			case "fix": {
				const fixes = e.problems.filter((p): p is Problem & { run: string } => !!p.run);
				if (fixes.length === 0) {
					setMessage({ text: "no fix to run here", color: "yellow" });
					return;
				}
				const confirmFix = (p: Problem & { run: string }) =>
					openOverlay({
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
				openOverlay({
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
						text: `${called(e)} has everything ${e.parent ? (names.get(e.parent) ?? labelBeside(e.parent, e.branch)) : "main"} has; ${called(s.from)} is behind: update it first (select it, m), or U updates all, parents first`,
						color: "yellow",
					});
					return;
				}
				if (e.kind === "branch") {
					// No checkout to run it in: from main's, by name.
					dryThenDo(
						["update", "--branch", e.branch, "--dry-run"],
						["update", "--branch", e.branch],
						mainRoot,
						`Merge ${e.parent ? (names.get(e.parent) ?? labelBeside(e.parent, e.branch)) : "main"} into ${called(e)}, without a checkout? (hydra update --branch)`,
						undefined,
						e.root,
					);
					return;
				}
				dryThenDo(
					["update", "--here", "--dry-run"],
					["update", "--here"],
					e.root,
					e.parent && e.stack.length > 1
						? `Merge ${names.get(e.parent) ?? labelBeside(e.parent, e.branch)} into ${called(e)}? (hydra update --here)`
						: `Merge main into ${called(e)}? (hydra update --here)`,
					e.root,
				);
				return;
			}
			case "tunnel": {
				const all = tunnelsHeld;
				if (all.length === 0) {
					setMessage({ text: "this stack has no tunnel (x-hydra tunnel on the tunnel's process)", color: "yellow" });
					return;
				}
				const move = ({ t, holder }: TunnelHeld) => {
					const title = `${t.name}${t.url ? ` (${t.url})` : ""}`;
					const back = e.kind === "main" || t.holder === e.root;
					if (back && !t.holder) {
						openOverlay(undefined);
						setMessage({ text: `tunnel ${title} goes to main already; select a worktree and t to take it there`, color: "yellow" });
						return;
					}
					if (back) {
						openOverlay({
							kind: "confirm",
							title: `Give tunnel ${title} back to main? (hydra tunnel release)`,
							lines: [`Its webhooks will reach main's ${t.to} instead of ${holder}'s.`],
							onYes: () => {
								openOverlay(undefined);
								runQuiet(["tunnel", "release", t.name], mainRoot, "main", "taking the tunnel back");
							},
						});
						return;
					}
					const own = e.services.some((s) => s.name === t.to && s.runs === "here" && !s.off);
					openOverlay({
						kind: "confirm",
						title: `Move tunnel ${title} from ${holder} to ${called(e)}? (hydra tunnel take)`,
						lines: [
							own
								? `Its webhooks will reach ${called(e)}'s ${t.to}.`
								: `Its webhooks will reach main's ${t.to}, tagged as ${called(e)}'s: ${called(e)} runs no ${t.to} of its own.`,
							...(e.up ? [] : [`${called(e)}'s stack isn't running: they get a 502 until u starts it.`]),
						],
						onYes: () => {
							openOverlay(undefined);
							runQuiet(["tunnel", "take", t.name], e.root, called(e), "taking the tunnel");
						},
					});
				};
				if (all.length === 1) return move(all[0]!);
				openOverlay({ kind: "pick", title: "Which tunnel?", items: all.map(({ t, holder }) => `${t.name} (now ${holder})`), index: 0, onPick: (i) => move(all[i]!) });
				return;
			}
			case "update":
				dryThenDo(
					["update", "--dry-run"],
					["update"],
					mainRoot,
					"Update everything? (hydra update: pulls main, restarts what moved, merges each branch's parent into it, parents first)",
				);
				return;
		}
	};

	/** A key, typed or clicked (the footer's, an overlay's). */
	const press = (input: string, key: Partial<Key>) => {
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
					else if (input === "n" || input === "N" || key.escape || input === "q") openOverlay(undefined);
					return;
				case "pick":
					if (key.upArrow || input === "k")
						openOverlay({ ...overlay, index: Math.max(0, overlay.index - 1) });
					else if (key.downArrow || input === "j")
						openOverlay({ ...overlay, index: Math.min(overlay.items.length - 1, overlay.index + 1) });
					else if (key.return) overlay.onPick(overlay.index);
					else if (key.escape || input === "q") openOverlay(undefined);
					return;
				default: {
					if (key.escape || input === "q" || key.return || input === "?") {
						openOverlay(undefined);
						return;
					}
					const g = geo.current;
					const by = key.pageDown ? 10 : key.pageUp ? -10 : key.downArrow || input === "j" ? 1 : key.upArrow || input === "k" ? -1 : 0;
					if (by && overlay.kind === "help") setHelpFrom((f) => clamp(f + by, 0, g?.helpMax ?? 0));
					if (by && overlay.kind === "output") setOutBack((b) => clamp(b - by, 0, g?.outputMax ?? 0));
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
		if (input === "?") return openOverlay({ kind: "help" });
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
			p: "pr",
			f: "fix",
			m: "merge",
			U: "update",
			t: "tunnel",
		};
		if (key.return) return act("open", sel);
		if (keys[input]) act(keys[input]!, sel);
	};
	useInput((input, key) => press(input, key));

	/** The mouse: a click chooses, presses or opens what is under it, the
	 *  wheel scrolls the panel under it, and a panel's border drags. */
	const onMouse = (m: MouseEvent) => {
		const g = geo.current;
		if (!g || m.mod) return;
		if (m.kind === "release") {
			dragging.current = undefined;
			return;
		}
		if (m.kind === "drag") {
			// The list's right border follows the pointer, or its bottom one.
			if (dragging.current === "width") setListWSet(m.x + 1);
			else if (dragging.current === "height") setListHSet(m.y - g.list.y + 1);
			return;
		}
		if (m.kind === "wheel") {
			const d = m.dir ?? 0;
			if (overlay?.kind === "help") return setHelpFrom((f) => clamp(f + d * 3, 0, g.helpMax));
			if (inside(g.list, m.x, m.y)) return overlay ? undefined : select(index + d);
			if (inside(g.problems, m.x, m.y)) return setPscroll((p) => clamp(p + d * 3, 0, g.problemsMax));
			if (inside(g.detail, m.x, m.y)) {
				if (overlay?.kind === "pick") return openOverlay({ ...overlay, index: clamp(overlay.index + d, 0, overlay.items.length - 1) });
				if (overlay?.kind === "output") return setOutBack((b) => clamp(b - d * 3, 0, g.outputMax));
				if (!overlay) setScroll((s) => clamp(Math.min(s, g.detailMax) + d * 3, 0, g.detailMax));
			}
			return;
		}
		if (m.kind !== "press" || m.button !== 0) return;
		if (overlay?.kind === "help") return openOverlay(undefined);
		// A border between the panels: drag it.
		const inBody = m.y >= g.body.y && m.y < g.body.y + g.body.h;
		if (g.wide && inBody && (m.x === g.list.w - 1 || m.x === g.list.w)) {
			dragging.current = "width";
			return;
		}
		const bottom = g.list.y + g.list.h - 1; // the list's bottom border; the panel under it starts below
		if ((!g.wide || m.x < g.list.w) && (m.y === bottom || m.y === bottom + 1)) {
			dragging.current = "height";
			return;
		}
		// The footer's keys.
		if (m.y === g.footerY) {
			const k = segAt(g.footer, size.columns, m.x)?.key;
			if (k) press(k === "⏎" ? "" : k, k === "⏎" ? { return: true } : {});
			return;
		}
		if (overlay) {
			if (!inside(g.detail, m.x, m.y)) return;
			const row = m.y - g.detail.y - 1;
			const room = g.detail.h - 2;
			if (overlay.kind === "pick") {
				const i = pickerStart(overlay.index, overlay.items.length, room) + row;
				if (row >= 0 && row < room - 1 && i < overlay.items.length) overlay.onPick(i);
				return;
			}
			if (row === room - 1) {
				// The overlay's own keys, on its last row (ConfirmBody, OutputBody).
				const segs: Seg[] =
					overlay.kind === "confirm"
						? [{ text: " y ", key: "y" }, { text: " yes, do it", key: "y" }, { text: "    " }, { text: "n / esc no", key: "n" }]
						: [{ text: "esc closes", key: "esc" }];
				const k = segAt(segs, g.detail.w - 4, m.x - g.detail.x - 2)?.key;
				if (k === "esc") press("", { escape: true });
				else if (k) press(k, {});
			}
			return;
		}
		if (inside(g.list, m.x, m.y)) {
			const i = m.y - g.list.y - 1;
			if (i >= 0 && i < g.visible) select(g.start + i);
			return;
		}
		// A link in the detail or the problems: a checkout to choose, or a URL.
		const follow = (rows: Seg[][], a: Area, inner: number) => {
			const link = linkAt(rows[m.y - a.y - 1] ?? [], inner, m.x - a.x - 2);
			if (link?.root) {
				const at = list.findIndex((e) => e.root === link.root);
				if (at >= 0) select(at);
			} else if (link?.url) {
				const url = link.url;
				void backend.browse(url).then((r) => setMessage(r.code === 0 ? { text: `opened ${url}` } : { text: r.output || `couldn't open ${url}`, color: "red" }));
			}
		};
		if (inside(g.detail, m.x, m.y)) follow(g.detailRows, g.detail, g.detailInner);
		else if (g.problems && inside(g.problems, m.x, m.y)) follow(g.problemRows, g.problems, g.problems.w - 4);
	};
	const mouseRef = useRef(onMouse);
	mouseRef.current = onMouse;
	useEffect(() => (mouse ? mouse((m) => mouseRef.current(m)) : undefined), [mouse]);

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
	// The PR column: as wide as its longest text, and only when the deepest
	// row keeps room for its name; it is the first thing to go.
	const prText = prWidth(list);
	let listW = wide ? Math.min(84, Math.max(60, Math.floor(width * 0.42))) : width;
	if (wide && prText) listW = Math.max(listW, Math.min(listW + prText + 1, width - 60));
	// A border dragged with the mouse wins, within what keeps both readable.
	if (wide && listWSet !== undefined) listW = clamp(listWSet, 36, width - 40);
	const maxLead = Math.max(0, ...nodes.map((n) => Array.from(n.lead).length));
	const prW = prText && rowColumns(listW - 2 - prText - 1, maxLead).name >= 10 ? prText : 0;
	const listH =
		listHSet !== undefined
			? clamp(listHSet, 3, body - 3)
			: wide
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
	const done = donePRCount(list);
	const anyPending = !!snap?.worktrees.some((w) => w.pending);

	// Where things are, for the mouse.
	const g: Geometry = {
		wide,
		list: { x: 0, y: 1, w: listW, h: listH },
		detail: wide ? { x: listW, y: 1, w: detailW, h: detailH } : { x: 0, y: 1 + listH, w: width, h: detailH },
		problems: wide ? { x: 0, y: 1 + listH, w: listW, h: body - listH } : undefined,
		body: { x: 0, y: 1, w: width, h: body },
		footerY: 1 + body + (status.length ? 1 : 0),
		footer: footerSegs({ e: sel, width, overlay: !!overlay, tunnel: tunnelsHeld.length > 0 }),
		start,
		visible,
		detailRows: [],
		detailInner,
		detailMax: 0,
		problemRows: [],
		problemsMax: 0,
		helpMax: Math.max(0, KEYS.length - (body - 3)),
		outputMax: 0,
	};
	geo.current = g;

	// The detail panel, or an overlay in its place.
	let title: Seg[];
	let color = "gray";
	let content: React.ReactNode;
	if (overlay?.kind === "help") {
		title = [{ text: "Keys", bold: true }];
		color = "cyan";
		content = <HelpBody width={detailInner} height={detailRoom} prs={snap?.prs} />;
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
		g.outputMax = Math.max(0, outputLines(overlay.text, detailInner).length - (detailRoom - 1));
		content = <OutputBody text={overlay.text} width={detailInner} height={detailRoom} back={outBack} />;
	} else if (sel) {
		// The worktrees stacked on a branch with no checkout run its code.
		const node = nodeOf.get(sel);
		const at = node ? nodes.indexOf(node) : -1;
		const via: Entry[] = [];
		for (let i = at + 1; node && i < nodes.length && nodes[i]!.depth > node.depth; i++) if (nodes[i]!.e.kind === "worktree") via.push(nodes[i]!.e);
		const lines = detailRows(sel, detailInner, { home, busy: verbOf(sel), problems: !wide, node, staleOf, prs: snap?.prs, via, tunnels: tunnelsHeld });
		const max = Math.max(0, lines.length - detailRoom);
		const from = Math.min(scroll, max);
		const shown = lines.slice(from, from + detailRoom);
		if (lines.length > from + detailRoom && shown.length > 0) {
			shown[shown.length - 1] = [{ text: `… ${lines.length - from - detailRoom + 1} more lines (pgdn)`, dim: true }];
		}
		title = detailTitle(sel, verbOf(sel));
		g.detailRows = shown;
		g.detailMax = max;
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
						<ListRow
							key={n.e.root}
							node={n}
							selected={n.e.root === sel?.root}
							width={listInner}
							busy={verbOf(n.e)}
							prW={prW}
							short={n.e.kind === "main" || names.get(n.e.branch) === shortBranch(n.e.branch) ? undefined : names.get(n.e.branch)}
						/>
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
		const maxP = Math.max(0, rowsP.length - room);
		const fromP = Math.min(pscroll, maxP);
		const shownP = rowsP.slice(fromP, fromP + room);
		if (rowsP.length > fromP + room) shownP[room - 1] = [{ text: `… ${rowsP.length - fromP - room + 1} more lines`, dim: true }];
		g.problemRows = shownP;
		g.problemsMax = maxP;
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
					...(done.count ? [{ text: " · " }, { text: `${done.count} on ${done.count === 1 ? "a " : ""}${done.closed ? "merged or closed" : "merged"} PR${done.count === 1 ? "" : "s"}`, color: "magenta", bold: true }] : []),
				]
			: [];
		const busyNote: Seg[] =
			reading || anyPending
				? [{ text: "   " }, { text: "", spin: true, color: "cyan" }, { text: reading ? " reading everything again" : " planning" }]
				: [];
		const repo: Seg[] = [{ text: snap ? "  " + snap.repo.slice(snap.repo.lastIndexOf("/") + 1) : "", bold: true }];
		// Where the tunnel's requests go: "⇄ tunnel: cart" (main's: dim).
		const tunnel: Seg[] = tunnelsHeld.length
			? [
					{ text: " · " },
					{ text: "⇄ ", color: "magenta", bold: true },
					{ text: `tunnel${tunnelsHeld.length > 1 ? "s" : ""}: ` },
					...tunnelsHeld.flatMap(({ t, holder }, i): Seg[] => [
						...(i ? [{ text: ", " }] : []),
						t.holder ? { text: holder, color: t.down ? "red" : "magenta", bold: true } : { text: holder, dim: true },
					]),
				]
			: [];
		const segs = (withRepo: boolean, withTunnel = true): Seg[] => [
			{ text: " hydra", bold: true, color: "magenta" },
			...(withRepo ? repo : []),
			...main,
			...(withTunnel ? tunnel : []),
			...busyNote,
		];
		const width_ = (x: Seg[]) => x.reduce((k, s) => k + (s.spin ? 1 : Array.from(s.text).length), 0);
		// Narrow: the repository's name goes first, it is the least news; then the tunnel.
		if (width_(segs(true)) <= w) return segs(true);
		return width_(segs(false)) <= w ? segs(false) : segs(false, false);
	}

	if (blank) return <Box />;
	return (
		<ThemeContext.Provider value={theme}>
			<Box flexDirection="column" width={width} height={rows}>
				<Row width={width} segs={header(width)} />
				{overlay?.kind === "help" ? (
					<Panel title={[{ text: "Keys", bold: true, color: undefined }]} color="cyan" width={width} height={body}>
						<Box flexDirection="column" paddingX={1}>
							<HelpBody width={width - 4} height={body - 2} prs={snap?.prs} from={helpFrom} />
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
				<Row width={width} segs={g.footer} />
			</Box>
		</ThemeContext.Provider>
	);
}
