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
	browsed: string[] = [];
	browse(url: string) {
		this.browsed.push(url);
		return Promise.resolve({ code: 0, output: "" });
	}
	terminal(cmd: TerminalCommand) {
		this.terminals.push(cmd);
		return 0;
	}
}

// A CI runner is slower than a laptop: React's renders and effects may not
// all be done in 30ms there, so its ticks are longer.
const slow = process.env["CI"] ? 10 : 1;
export const tick = (ms = 30) => new Promise((r) => setTimeout(r, ms * slow));

// eslint-disable-next-line no-control-regex
export const plain = (s: string | undefined) => (s ?? "").replace(/\x1b\[[0-9;]*m/g, "");
