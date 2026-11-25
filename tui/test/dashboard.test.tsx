import { test } from "node:test";
import assert from "node:assert/strict";
import React from "react";
import { render } from "ink-testing-library";
import App from "../src/App.js";
import { Confirm, Footer, ListRow, columns, detailLines } from "../src/ui.js";
import { entries, fixCommand, stackState, vsMain, wrap } from "../src/model.js";
import { FakeBackend, plain, tick } from "./fake.js";
import { fixture } from "./fixture.js";

const DOWN = "\u001B[B";
const ENTER = "\r";

function app(width = 80, rows = 30) {
	const backend = new FakeBackend(fixture());
	const r = render(<App backend={backend} size={{ columns: width, rows }} />);
	return { backend, ...r, frame: () => plain(r.lastFrame()) };
}

function noLineWiderThan(frame: string, width: number) {
	for (const line of frame.split("\n")) {
		assert.ok(Array.from(line).length <= width, `wider than ${width}: ${JSON.stringify(line)}`);
	}
}

test("the list shows main and every worktree, with where each stands", async () => {
	const { frame, unmount } = app();
	await tick();
	const f = frame();
	assert.match(f, /main\s+0\s+up\s+3\/4 ok\s+2↓ upstream/);
	assert.match(f, /▸\* cart\s+1\s+up ✗2|▸\* cart\s+1\s+failing/, "the checkout hydra ran in is chosen, and marked");
	assert.match(f, /clash\s+-\s+down\s+api\s+conflicts\(1\) 1↓ 1↑/);
	assert.match(f, /more\s+2\s+down\s+web\s+2↑ on cart-checkout/);
	assert.match(f, /fresh\s+-\s+…/);
	assert.match(f, /main up · 1\/4 worktrees up/);
	unmount();
});

test("the detail lists what runs here, what comes from main, and the problems with their fixes", async () => {
	const { frame, unmount } = app(100, 40);
	await tick();
	const f = frame();
	assert.match(f, /cart\s+santiago\/cart-checkout · slot 1 · stack up/);
	assert.match(f, /3 behind main, 1 ahead · update: main would be merged in \(3 commits\)/);
	assert.match(f, /● web\s+here\s+3100\s+running/);
	assert.match(f, /✗ worker\s+here\s+8120\s+exited with code 1/);
	assert.match(f, /● api\s+main\s+8080\s+ready/);
	assert.doesNotMatch(f, /seed/, "main's services nothing here uses are counted, not listed");
	assert.match(f, /1 more of main's services aren't used/);
	assert.match(f, /✗ this branch is behind main/);
	assert.match(f, /fix: hydra update   \(or, for this worktree alone: git merge main\)\s+\(f runs it\)/);
	assert.match(f, /fix: hydra process restart worker\s+\(f runs it\)/);
	unmount();
});

test("detail lines of a conflicting and of a stacked worktree", () => {
	const list = entries(fixture());
	const text = (i: number) =>
		render(<>{detailLines(list[i]!, 80, "main")}</>).lastFrame() ?? "";
	assert.match(plain(text(2)), /conflicting: apps\/api\/users.go/);
	assert.match(plain(text(2)), /✓ nothing in the way \(its stack isn't running: u starts it\)/);
	assert.match(plain(text(3)), /stacked: main ← cart-checkout ← more/);
	assert.match(plain(text(4)), /planning…/);
});

test("the footer shows the keys, merge only for a worktree", () => {
	const wt = plain(render(<Footer kind="worktree" width={80} />).lastFrame());
	assert.match(wt, /⏎ open\s+u up\s+d down\s+r restart\s+l logs\s+m merge\s+U update\s+\? help\s+q quit/);
	noLineWiderThan(wt, 80);
	const main = plain(render(<Footer kind="main" width={120} />).lastFrame());
	assert.doesNotMatch(main, /merge/);
	assert.match(main, /f fix · U update all/);
});

test("a confirm dialog says what it will do, and how to say no", () => {
	const f = plain(render(<Confirm title="Stop main's stack? (hydra down)" lines={["Every worktree loses it."]} height={6} width={80} />).lastFrame());
	assert.match(f, /Stop main's stack\? \(hydra down\)/);
	assert.match(f, /Every worktree loses it\./);
	assert.match(f, /y yes, do it · n\/esc no/);
});

test("d asks first, then runs hydra down in that worktree", async () => {
	const { backend, stdin, frame, unmount } = app();
	await tick();
	stdin.write("d");
	await tick();
	assert.match(frame(), /Stop cart's stack\? \(hydra down\)/);
	assert.equal(backend.runs.length, 0, "nothing runs before the yes");
	stdin.write("y");
	await tick();
	assert.deepEqual(backend.runs, [{ args: ["down"], cwd: "/repo/cart" }]);
	assert.match(frame(), /✓ hydra down in cart: ok/);
	unmount();
});

test("n leaves things as they were", async () => {
	const { backend, stdin, frame, unmount } = app();
	await tick();
	stdin.write("d");
	await tick();
	stdin.write("n");
	await tick();
	assert.equal(backend.runs.length, 0);
	assert.doesNotMatch(frame(), /Stop cart/);
	unmount();
});

test("u starts the chosen checkout's stack; a refusal shows what hydra said", async () => {
	const { backend, stdin, frame, unmount } = app();
	backend.results["up -D"] = { code: 1, output: "✗ main's stack isn't running.\nhydra: not starting this worktree\n" };
	await tick();
	stdin.write(DOWN); // clash
	await tick();
	stdin.write("u");
	await tick();
	assert.deepEqual(backend.runs, [{ args: ["up", "-D"], cwd: "/repo/clash" }]);
	assert.match(frame(), /✗ exit 1: hydra up -D in clash/);
	assert.match(frame(), /not starting this worktree/);
	unmount();
});

test("enter opens the stack with hydra attach, in that checkout", async () => {
	const { backend, stdin, unmount } = app();
	await tick();
	stdin.write(ENTER);
	await tick();
	assert.equal(backend.terminals.length, 1);
	assert.deepEqual(backend.terminals[0]!.argv, ["hydra", "attach"]);
	assert.equal(backend.terminals[0]!.cwd, "/repo/cart");
	assert.deepEqual(backend.refreshes, ["/repo/cart"]);
	unmount();
});

test("enter on a stack that isn't running says how to start it", async () => {
	const { backend, stdin, frame, unmount } = app();
	await tick();
	stdin.write(DOWN);
	await tick();
	stdin.write(ENTER);
	await tick();
	assert.equal(backend.terminals.length, 0);
	assert.match(frame(), /clash's stack isn't running: u starts it/);
	unmount();
});

test("m shows the dry run, and on yes runs hydra update --here in the terminal", async () => {
	const { backend, stdin, frame, unmount } = app();
	backend.results["update --here --dry-run"] = {
		code: 0,
		output: "(dry run: nothing is pulled, restarted or merged)\n\nworktrees\n  cart  cart  main would be merged in (3 commits)\n",
	};
	await tick();
	stdin.write("m");
	await tick();
	assert.match(frame(), /Merge main into cart\? \(hydra update --here\)/);
	assert.match(frame(), /main would be merged in \(3 commits\)/);
	stdin.write("y");
	await tick();
	assert.deepEqual(backend.terminals[0]!.argv, ["hydra", "update", "--here"]);
	assert.equal(backend.terminals[0]!.cwd, "/repo/cart");
	unmount();
});

test("U runs the dry run in the main checkout first", async () => {
	const { backend, stdin, frame, unmount } = app();
	await tick();
	stdin.write("U");
	await tick();
	assert.deepEqual(backend.runs, [{ args: ["update", "--dry-run"], cwd: "/repo/main" }]);
	assert.match(frame(), /Update everything\?/);
	stdin.write("y");
	await tick();
	assert.deepEqual(backend.terminals[0]!.argv, ["hydra", "update"]);
	unmount();
});

test("f picks a fix, asks, and runs it with this hydra", async () => {
	const { backend, stdin, frame, unmount } = app();
	await tick();
	stdin.write("f");
	await tick();
	assert.match(frame(), /Run which fix\?/);
	stdin.write(DOWN);
	await tick();
	stdin.write(ENTER);
	await tick();
	assert.match(frame(), /Run in cart: hydra process restart worker/);
	stdin.write("y");
	await tick();
	assert.equal(backend.terminals[0]!.shell, "/usr/local/bin/hydra process restart worker");
	assert.equal(backend.terminals[0]!.cwd, "/repo/cart");
	unmount();
});

test("r and l pick a service", async () => {
	const { backend, stdin, frame, unmount } = app();
	await tick();
	stdin.write("r");
	await tick();
	assert.match(frame(), /Restart which of cart's services\?/);
	stdin.write(ENTER);
	await tick();
	assert.deepEqual(backend.runs[0], { args: ["process", "restart", "web"], cwd: "/repo/cart" });
	await tick();
	stdin.write("l");
	await tick();
	assert.match(frame(), /api \(main's\)/);
	stdin.write(DOWN);
	await tick();
	stdin.write(DOWN);
	await tick();
	stdin.write(ENTER);
	await tick();
	const t = backend.terminals[0]!;
	assert.deepEqual(t.argv, ["hydra", "process", "logs", "api", "-f", "-n", "300"]);
	assert.equal(t.cwd, "/repo/main", "main's copy's logs come from main's stack");
	unmount();
});

test("? shows the keys, and how to come back from an open stack", async () => {
	const { stdin, frame, unmount } = app();
	await tick();
	stdin.write("?");
	await tick();
	assert.match(frame(), /F10 \(or ctrl-c\) then Yes comes back; the stack keeps running/);
	stdin.write("\u001B");
	await tick();
	assert.doesNotMatch(frame(), /F10/);
	unmount();
});

test("no line is wider than the terminal, narrow or wide", async () => {
	for (const width of [80, 100, 160]) {
		const { frame, unmount } = app(width, 30);
		await tick();
		noLineWiderThan(frame(), width);
		const f = frame();
		assert.equal(f.split("\n").length, 29, `the frame is one row short of the terminal at ${width}`);
		if (width >= 110) assert.match(f, /BRANCH/);
		else assert.doesNotMatch(f, /BRANCH/);
		unmount();
	}
});

test("a list row at 80 columns", () => {
	const e = entries(fixture())[1]!;
	const f = plain(render(<ListRow e={e} selected={false} cols={columns(80)} />).lastFrame());
	noLineWiderThan(f, 80);
	assert.equal(stackState(e).text, "failing");
	assert.equal(vsMain(e).text, "behind in api 3↓ 1↑");
});

test("fixes run with the dashboard's hydra", () => {
	assert.equal(fixCommand("hydra update", "/x/hydra"), "/x/hydra update");
	assert.equal(fixCommand("cd '/a b' && hydra up -D", "/x y/hydra"), "cd '/a b' && '/x y/hydra' up -D");
	assert.equal(fixCommand("hydra process restart hydra-watch", "/x/hydra"), "/x/hydra process restart hydra-watch");
});

test("wrap keeps words whole and carries indentation", () => {
	assert.deepEqual(wrap("one two three four", 9), ["one two", "  three", "  four"]);
	assert.deepEqual(wrap("  api: a b", 8), ["  api: a", "    b"]);
	assert.ok(wrap("x".repeat(30), 10).every((l) => l.length <= 10));
});

test("keys typed together all count", async () => {
	const { stdin, frame, unmount } = app();
	await tick();
	stdin.write("jj");
	await tick();
	assert.match(frame(), /▸  more/);
	stdin.write("kkk");
	await tick();
	assert.match(frame(), /▸  main/);
	unmount();
});

test("a worktree hydra stopped for being idle says so", () => {
	const snap = fixture();
	snap.worktrees[2]!.idleStopped = { at: new Date().toISOString(), after: "2h" };
	const e = entries(snap)[3]!;
	assert.equal(stackState(e).text, "idle-off");
	const row = plain(render(<ListRow e={e} selected={false} cols={columns(80)} />).lastFrame());
	noLineWiderThan(row, 80);
	assert.match(row, /more\s+2\s+idle-off\s+web/);
	const detail = plain(render(<>{detailLines(e, 80, "main")}</>).lastFrame() ?? "");
	assert.match(detail, /stopped after 2h idle/);
});
