// Everything the dashboard does to the outside world goes through here:
// the snapshot stream, and the hydra commands its keys run. The app gets a
// Backend, so tests can hand it a fake one.

import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { createInterface } from "node:readline";
import type { Snapshot } from "./types.js";
import { shQuote } from "./model.js";
import { ASK, REPORT_OFF, REPORT_ON } from "./theme.js";

export interface Result {
	code: number;
	output: string;
}

export interface Backend {
	/** The hydra the dashboard runs, for commands shown and run. */
	exe: string;
	/** Starts the snapshot stream. Returns a function that stops it. */
	watch(onSnapshot: (s: Snapshot) => void, onError: (msg: string) => void): () => void;
	/** Asks for everything (or one worktree) to be read again now. */
	refresh(root?: string): void;
	/** Runs hydra in cwd, in the background, and collects what it printed. */
	run(args: string[], cwd: string): Promise<Result>;
	/** Hands the terminal to a command and returns when it ends. pause waits
	 *  for a key first, so its output can be read. */
	terminal(cmd: TerminalCommand): number;
	/** Opens an address in the browser. */
	browse(url: string): Promise<Result>;
}

export interface TerminalCommand {
	/** argv; argv[0] "hydra" means this dashboard's hydra. */
	argv?: string[];
	/** a shell command line, instead of argv */
	shell?: string;
	cwd: string;
	title: string;
	/** wait for a key before going back: always, or only when it failed */
	pause: boolean | "failed";
}

export const ESC = {
	altOn: "\x1b[?1049h",
	altOff: "\x1b[?1049l",
	cursorOff: "\x1b[?25l",
	cursorOn: "\x1b[?25h",
	clear: "\x1b[2J\x1b[H",
};

/** Called after a terminal command, to have Ink draw the whole frame again. */
let redraw: () => void = () => {};
export function onRedraw(fn: () => void) {
	redraw = fn;
}

export function realBackend(exe: string, cwd: string): Backend {
	let child: ChildProcess | undefined;
	return {
		exe,
		watch(onSnapshot, onError) {
			// Its own process group: a ctrl-c typed at a command the dashboard
			// handed the terminal to must not reach it.
			child = spawn(exe, ["status", "--json", "--watch"], {
				cwd,
				stdio: ["pipe", "pipe", "pipe"],
				detached: true,
			});
			let stderr = "";
			child.stderr?.on("data", (d: Buffer) => {
				stderr = (stderr + d.toString()).slice(-2000);
			});
			const lines = createInterface({ input: child.stdout! });
			lines.on("line", (line) => {
				try {
					onSnapshot(JSON.parse(line) as Snapshot);
				} catch {
					onError(`unreadable snapshot: ${line.slice(0, 80)}`);
				}
			});
			child.on("exit", (code) => {
				if (code !== null) onError(`hydra status --watch ended (${code}): ${stderr.trim() || "no output"}`);
			});
			child.on("error", (e) => onError(`can't run ${exe}: ${e.message}`));
			return () => {
				child?.stdin?.end();
				child?.kill();
			};
		},
		refresh(root) {
			child?.stdin?.write(root ? `refresh ${root}\n` : "refresh\n");
		},
		run(args, dir) {
			return new Promise((resolve) => {
				const p = spawn(exe, args, { cwd: dir, stdio: ["ignore", "pipe", "pipe"], detached: true });
				let output = "";
				p.stdout?.on("data", (d: Buffer) => (output += d.toString()));
				p.stderr?.on("data", (d: Buffer) => (output += d.toString()));
				p.on("error", (e) => resolve({ code: 127, output: `can't run ${exe}: ${e.message}` }));
				p.on("close", (code) => resolve({ code: code ?? 1, output }));
			});
		},
		terminal(cmd) {
			return handOver(exe, cmd);
		},
		browse(url) {
			const opener = process.platform === "darwin" ? "open" : "xdg-open";
			return new Promise((resolve) => {
				const p = spawn(opener, [url], { stdio: "ignore", detached: true });
				p.on("error", (e) => resolve({ code: 127, output: `can't run ${opener}: ${e.message}` }));
				p.on("close", (code) => resolve({ code: code ?? 1, output: "" }));
			});
		},
	};
}

/**
 * Suspends the dashboard and gives the terminal to a command (santree's
 * pattern): leave the alternate screen and raw mode, run it with the
 * terminal as its stdio, then take both back and draw everything again.
 * The event loop stops meanwhile, so Ink draws nothing over it.
 */
function handOver(exe: string, cmd: TerminalCommand): number {
	const out = process.stdout;
	const stdin = process.stdin;
	const wasRaw = stdin.isTTY ? stdin.isRaw : false;
	// The command gets the terminal's keys, so no theme report may land
	// among them; the dashboard asks again when it is back.
	out.write(REPORT_OFF + ESC.clear + ESC.altOff + ESC.cursorOn);
	if (stdin.isTTY) {
		try {
			stdin.setRawMode(false);
		} catch {}
	}
	out.write(`\x1b[2m$ ${cmd.title}   (in ${cmd.cwd})\x1b[0m\n`);
	// A ctrl-c is the command's (the end of `logs -f`), not the dashboard's.
	const ignore = () => {};
	process.on("SIGINT", ignore);
	let status: number;
	try {
		const env = { ...process.env, HYDRA_EXE: exe };
		const r = cmd.shell
			? spawnSync("/bin/sh", ["-c", cmd.shell], { cwd: cmd.cwd, stdio: "inherit", env })
			: spawnSync(cmd.argv![0] === "hydra" ? exe : cmd.argv![0]!, cmd.argv!.slice(1), {
					cwd: cmd.cwd,
					stdio: "inherit",
					env,
				});
		status = r.status ?? 1;
		if (r.error) out.write(`\n${r.error.message}\n`);
		if (cmd.pause === true || (cmd.pause === "failed" && status !== 0)) {
			const said = status === 0 ? "done" : `exit status ${status}`;
			spawnSync(
				"/bin/sh",
				[
					"-c",
					`printf '\\n\\033[2m%s · press any key to go back to the dashboard\\033[0m' ${shQuote(said)}; ` +
						"stty -icanon -echo; dd bs=1 count=1 >/dev/null 2>&1; stty icanon echo",
				],
				{ stdio: "inherit" },
			);
		}
	} finally {
		process.off("SIGINT", ignore);
		if (stdin.isTTY) {
			try {
				stdin.setRawMode(wasRaw);
			} catch {}
		}
		out.write(ESC.altOn + ESC.cursorOff + ESC.clear + REPORT_ON + ASK);
		redraw();
	}
	return status;
}
