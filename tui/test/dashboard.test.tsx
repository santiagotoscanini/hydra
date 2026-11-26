import { test } from "node:test";
import assert from "node:assert/strict";
import React from "react";
import { render } from "ink-testing-library";
import App from "../src/App.js";
import { ConfirmBody, Footer, ListRow, detailRows, footerKeys, problemRows, Row } from "../src/ui.js";
import { branchName, entries, fixCommand, stateOf, vsMain, wrap } from "../src/model.js";
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

/** Renders once, and unmounts, so no spinner keeps the test running. */
function once(el: React.ReactElement): string {
	const r = render(el);
	const f = plain(r.lastFrame());
	r.unmount();
	return f;
}

const rows = (segs: ReturnType<typeof detailRows>, width = 80) =>
	once(<>{segs.map((s, i) => <Row key={i} segs={s} width={width} />)}</>);

test("a branch is named without its user prefix, by its ticket and title", () => {
	assert.deepEqual(branchName("santiago/fla-709-an-advisor-splits"), { id: "fla-709", title: "an advisor splits" });
	assert.deepEqual(branchName("SCRM-264_app_ring"), { id: "scrm-264", title: "app ring" });
	assert.deepEqual(branchName("santiago/supabase-native-local"), { title: "supabase-native-local" });
	assert.deepEqual(branchName("main"), { title: "main" });
});

test("the list shows main and every worktree, with where each stands", async () => {
	const { frame, unmount } = app();
	await tick();
	const f = frame();
	assert.match(f, /● main\s+up\s+2 behind upstream/);
	assert.match(f, /▌✗ cart-checkout\s+failing\s+3 behind · needs main/, "the checkout hydra ran in is chosen");
	assert.match(f, /○ clash\s+down\s+1 behind · 1 conflict/);
	assert.match(f, /○ more\s+down\s+on cart-checkout/);
	assert.match(f, /fresh\s+planning/);
	assert.match(f, /main up · 1 of 4 worktrees up/);
	assert.doesNotMatch(f, /read now|SLOT|CHECKOUT/);
	unmount();
});

test("the detail lists what runs here, what comes from main, and the problems with their fixes", async () => {
	const { frame, unmount } = app(100, 44);
	await tick();
	const f = frame();
	assert.match(f, /╭─ cart-checkout {2}✗ failing/);
	assert.match(f, /santiago\/cart-checkout · \/repo\/cart · 300M/);
	assert.match(f, /⇅ vs main {2}3 behind · 1 ahead/);
	assert.match(f, /update: main would be merged in \(3 commits\)/);
	assert.match(f, /◆ Runs here {2}1\/2 ready/);
	assert.match(f, /● web\s+running\s+http:\/\/cart\.localhost:3900\s+changed: apps\/web\/cart\.tsx/);
	assert.match(f, /✗ worker\s+exited with code 1\s+:8120/);
	assert.match(f, /◇ From main {2}2 used, 1 more unused/);
	assert.match(f, /● api ● db/);
	assert.doesNotMatch(f, /seed/, "main's services nothing here uses are counted, not listed");
	assert.match(f, /! 2 problems/);
	assert.match(f, /✗ this branch is behind main/);
	assert.match(f, /fix hydra update {3}\(or, for this worktree alone: git merge main\) {3}f runs it/);
	assert.match(f, /fix hydra process restart worker {3}f runs it/);
	unmount();
});

test("a wide terminal shows the problems in a panel of their own, under the list", async () => {
	const { frame, unmount } = app(160, 40);
	await tick();
	const f = frame();
	assert.match(f, /╭─ Checkouts 5 ─+╮╭─ cart-checkout {2}✗ failing/);
	assert.match(f, /╭─ Problems 2 ─/);
	assert.match(f, /│ ✗ worker exited with code 1\./);
	noLineWiderThan(f, 160);
	unmount();
});

test("o opens the checkout's address by name in the browser", async () => {
	const { backend, stdin, frame, unmount } = app(100, 50);
	await tick();
	assert.match(frame(), /↗ Addresses {2}o opens one/);
	stdin.write("o");
	await tick();
	assert.deepEqual(backend.browsed, ["http://cart.localhost:3900"]);
	assert.match(frame(), /opened http:\/\/cart\.localhost:3900/);
	unmount();
});

test("detail of a conflicting, a stacked and a still planned worktree", () => {
	const list = entries(fixture());
	const all = (i: number) => rows([...detailRows(list[i]!, 80), ...problemRows(list[i]!, 80)]);
	assert.match(all(2), /merging main conflicts in 1 file: users\.go/);
	assert.match(all(2), /✓ nothing in the way: u starts it/);
	assert.match(all(3), /stacked on cart-checkout {2}\(main ← cart-checkout ← more\)/);
	assert.match(all(4), /working out what runs here/);
});

test("main lists what it runs even while its stack is down", () => {
	const snap = fixture();
	const m = snap.main!;
	m.up = false;
	m.processes = m.processes.map((p) => ({ ...p, state: "not running", health: "off" as const }));
	const e = entries(snap)[0]!;
	assert.deepEqual(stateOf(e), { glyph: "○", spin: false, word: "down", color: "red" });
	const f = rows(detailRows(e, 80));
	assert.match(f, /not running: the worktrees that use main's services need it/);
	assert.match(f, /◆ Services {2}4, none running/);
	assert.match(f, /○ api\s+not running\s+:8080/);
	assert.doesNotMatch(f, /nothing runs here|switched off/);
});

test("a stack being started says how far it got, in the row and the panel", () => {
	const snap = fixture();
	snap.worktrees[0]!.services[1]!.health = "starting";
	const e = entries(snap)[1]!;
	assert.equal(stateOf(e).word, "starting 1/2");
	assert.equal(stateOf(e, "starting").word, "starting 1/2");
	assert.equal(stateOf(entries(fixture())[2]!, "starting").word, "starting…", "before the stack answers");
	assert.equal(stateOf(entries(fixture())[2]!, "stopping").spin, true);
});

test("the footer shows only the keys that apply to the selection", () => {
	const list = entries(fixture());
	const keys = (i: number) => footerKeys(list[i]).map(([k]) => k).join(" ");
	assert.equal(keys(0), "⏎ d r l o f", "main is up: no up, no merge");
	assert.equal(keys(1), "⏎ d r l o f m");
	assert.equal(keys(2), "u m", "down, nothing to fix, no logs to follow");
	assert.equal(keys(4), "", "still being planned");
	for (const width of [80, 120]) {
		const f = once(<Footer e={list[1]} width={width} />);
		noLineWiderThan(f, width);
		assert.match(f, /U update/);
		assert.match(f, /q quit/);
	}
	const f = once(<Footer e={list[1]} width={80} overlay />);
	assert.doesNotMatch(f, /quit/, "an overlay says its own keys");
});

test("a confirm dialog says what it will do, and how to say no", () => {
	const f = once(<ConfirmBody lines={["Every worktree loses it."]} height={6} width={80} />);
	assert.match(f, /Every worktree loses it\./);
	assert.match(f, /y {2}yes, do it {4}n \/ esc no/);
});

test("d asks first, then runs hydra down in that worktree", async () => {
	const { backend, stdin, frame, unmount } = app();
	await tick();
	stdin.write("d");
	await tick();
	assert.match(frame(), /Stop cart-checkout's stack\? \(hydra down\)/);
	assert.equal(backend.runs.length, 0, "nothing runs before the yes");
	stdin.write("y");
	await tick();
	assert.deepEqual(backend.runs, [{ args: ["down"], cwd: "/repo/cart" }]);
	assert.match(frame(), /✓ hydra down in cart-checkout: ok/);
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
	assert.match(frame(), /╭─ cart-checkout {2}✗ failing/, "the detail is back");
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
	assert.match(frame(), /Merge main into cart-checkout\? \(hydra update --here\)/);
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
	assert.match(frame(), /Run in cart-checkout: hydra process restart worker/);
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
	assert.match(frame(), /Restart which of cart-checkout's services\?/);
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
	assert.match(frame(), /F10 or ctrl-c, then Yes, comes back; the stack keeps running/);
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
		if (width >= 120) assert.match(f, /╭─ Problems/);
		else assert.doesNotMatch(f, /╭─ Problems/);
		unmount();
	}
});

test("a list row at 80 columns", () => {
	const e = entries(fixture())[1]!;
	const f = once(<ListRow e={e} selected width={78} selectionBg="#1e3a5f" />);
	noLineWiderThan(f, 78);
	assert.equal(stateOf(e).word, "failing");
	assert.deepEqual(vsMain(e), { text: "3 behind · needs main", color: "yellow" });
	assert.match(f, /^▌✗ cart-checkout/);
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
	assert.match(frame(), /▌○ more/);
	stdin.write("kkk");
	await tick();
	assert.match(frame(), /▌● main/);
	unmount();
});

test("a worktree hydra stopped for being idle says so", () => {
	const snap = fixture();
	snap.worktrees[2]!.idleStopped = { at: new Date().toISOString(), after: "2h" };
	const e = entries(snap)[3]!;
	assert.equal(stateOf(e).word, "idle-stopped");
	const row = once(<ListRow e={e} selected={false} width={78} selectionBg="#1e3a5f" />);
	noLineWiderThan(row, 78);
	assert.match(row, /◌ more\s+idle-stopped\s+on cart-checkout/);
	assert.match(rows(detailRows(e, 80)), /nobody used it for 2h, so hydra stopped it: u starts it/);
});

test("a worktree far behind main is marked, and the row still fits 80 columns", () => {
	const snap = fixture();
	const w = snap.worktrees[0]!;
	w.behind = 312;
	w.farBehind = { behind: 312, limit: 100, conflicts: ["apps/api/users.go", "apps/web/cart.tsx"] };
	w.update = undefined;
	const e = entries(snap)[1]!;
	assert.deepEqual(vsMain(e), { text: "312 behind · 2 conflicts", color: "red" });
	const row = once(<ListRow e={e} selected={false} width={78} selectionBg="#1e3a5f" />);
	noLineWiderThan(row, 78);
	assert.match(row, /312 behind · 2 conflicts/);
	assert.match(rows(detailRows(e, 80)), /merging main conflicts in 2 files: users\.go, cart\.tsx/);

	w.farBehind = { behind: 312, limit: 100 };
	w.outOfStep = [];
	assert.deepEqual(vsMain(entries(snap)[1]!), { text: "312 behind", color: "yellow" });
	assert.match(rows(detailRows(entries(snap)[1]!, 80)), /far behind: 312 commits, and the merge only gets harder/);
});
