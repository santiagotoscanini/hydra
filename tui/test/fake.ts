import type { Backend, Result, TerminalCommand } from "../src/hydra.js";
import type { Snapshot } from "../src/types.js";

/** A backend that records what the dashboard asked of it. */
export class FakeBackend implements Backend {
	exe = "/usr/local/bin/hydra";
	runs: Array<{ args: string[]; cwd: string }> = [];
	terminals: TerminalCommand[] = [];
	refreshes: Array<string | undefined> = [];
	results: Record<string, Result> = {};
	push: (s: Snapshot) => void = () => {};

	constructor(private snap: Snapshot) {}

	watch(onSnapshot: (s: Snapshot) => void) {
		this.push = onSnapshot;
		onSnapshot(this.snap);
		return () => {};
	}
	refresh(root?: string) {
		this.refreshes.push(root);
	}
	run(args: string[], cwd: string) {
		this.runs.push({ args, cwd });
		return Promise.resolve(this.results[args.join(" ")] ?? { code: 0, output: "ok\n" });
	}
	terminal(cmd: TerminalCommand) {
		this.terminals.push(cmd);
		return 0;
	}
}

export const tick = (ms = 30) => new Promise((r) => setTimeout(r, ms));

// eslint-disable-next-line no-control-regex
export const plain = (s: string | undefined) => (s ?? "").replace(/\x1b\[[0-9;]*m/g, "");
