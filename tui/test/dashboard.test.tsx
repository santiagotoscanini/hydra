import { test } from "node:test";
import assert from "node:assert/strict";
import React from "react";
import { render } from "ink-testing-library";
import App from "../src/App.js";
import { ConfirmBody, Footer, ListRow, changedRows, detailRows, footerKeys, problemRows, Row } from "../src/ui.js";
import { badgeOf, branchName, changedIn, entries, fitPaths, fixCommand, stateOf, tree, wrap, type Entry, type Node } from "../src/model.js";
import type { Branch, Snapshot, WorktreeState } from "../src/types.js";
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

/** The tree node of the worktree in dir. */
function nodeOf(snap: Snapshot, dir: string): Node {
	return tree(entries(snap)).find((n) => n.e.name === dir)!;
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
	assert.match(f, /▣ main\s+up\s+2 behind upstream/);
	assert.match(f, /▌├─✗ cart-checkout\s+failing\s+3 behind main/, "the checkout hydra ran in is chosen");
	assert.match(f, /│ └─▣ more\s+down\s+↑ cart-checkout is behind/, "under the branch it is stacked on");
	assert.match(f, /├─▣ clash\s+down\s+1 behind main · 1 conflict/);
	assert.match(f, /└─. fresh\s+planning/);
	assert.match(f, /main up, 2 behind upstream \(U\) · 1 of 4 worktrees up · 3 out of date/);
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
	assert.match(f, /◆ Runs here, and why {2}1\/2 ready/);
	assert.match(f, /● web\s+running\s+http:\/\/cart\.localhost:3900\s*│\n│\s+↳ changed: apps\/web\/cart\.tsx/);
	assert.match(f, /✗ worker\s+exited with code 1\s+:8120/);
	assert.match(f, /◇ From main {2}● api {2}● db · 1 unused/);
	// In this order: the chain, what runs here and why, main's, the problems.
	assert.ok(f.indexOf("⇅ vs main") < f.indexOf("◆ Runs here") && f.indexOf("◆ Runs here") < f.indexOf("◇ From main") && f.indexOf("◇ From main") < f.indexOf("! 2 problems"));
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
	stdin.write("o");
	await tick();
	assert.deepEqual(backend.browsed, ["http://cart.localhost:3900"]);
	assert.match(frame(), /opened http:\/\/cart\.localhost:3900/);
	unmount();
});

test("detail of a conflicting, a stacked and a still planned worktree", () => {
	const list = entries(fixture());
	const nodes = tree(list);
	const node = (i: number) => nodes.find((n) => n.e === list[i])!;
	const staleOf = (e: (typeof list)[number]) => nodes.find((n) => n.e === e)!.stale;
	const all = (i: number) => rows([...detailRows(list[i]!, 80, { node: node(i), staleOf }), ...problemRows(list[i]!, 80)]);
	assert.match(all(2), /merging main conflicts in 1 file: users\.go/);
	assert.match(all(2), /✓ nothing in the way: u starts it/);
	assert.match(all(3), /⇅ vs cart-checkout {2}in step {3}vs main: 2 ahead/);
	assert.match(all(3), /main → cart-checkout \(3 behind\) → more/);
	assert.match(all(3), /cart-checkout is behind main: update it first \(select it, m\), or U updates\s+every branch, parents first/);
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
	assert.match(f, /◆ Runs {2}4, none running/);
	assert.match(f, /⇅ Upstream {2}2 commits behind its upstream/);
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
	assert.equal(keys(3), "u", "in step with its parent: nothing to merge");
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
	stdin.write(DOWN); // more, under cart-checkout
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
	const n = nodeOf(fixture(), "cart");
	const f = once(<ListRow node={n} selected width={78} />);
	noLineWiderThan(f, 78);
	assert.equal(stateOf(n.e).word, "failing");
	assert.deepEqual(badgeOf(n), { texts: ["3 behind main", "3 behind"], color: "yellow", bold: true });
	assert.match(f, /^▌├─✗ cart-checkout/);
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
	assert.match(frame(), /▌├─▣ clash/, "down the tree as it reads: more, under cart-checkout, then clash");
	stdin.write("kkk");
	await tick();
	assert.match(frame(), /▌▣ main/);
	unmount();
});

test("a worktree hydra stopped for being idle says so", () => {
	const snap = fixture();
	snap.worktrees[2]!.idleStopped = { at: new Date().toISOString(), after: "2h" };
	const n = nodeOf(snap, "more");
	const e = n.e;
	assert.equal(stateOf(e).word, "idle-stopped");
	const row = once(<ListRow node={n} selected={false} width={78} />);
	noLineWiderThan(row, 78);
	assert.match(row, /▣ more\s+idle-stopped\s+↑ cart-checkout is behind/);
	assert.match(rows(detailRows(e, 80)), /nobody used it for 2h, so hydra stopped it: u starts it/);
});

test("a worktree far behind main is marked, and the row still fits 80 columns", () => {
	const snap = fixture();
	const w = snap.worktrees[0]!;
	w.behind = 312;
	w.farBehind = { behind: 312, limit: 100, conflicts: ["apps/api/users.go", "apps/web/cart.tsx"] };
	w.update = undefined;
	const n = nodeOf(snap, "cart");
	const e = n.e;
	assert.deepEqual(badgeOf(n).texts, ["312 behind main · 2 conflicts", "312 behind · 2 conflicts"]);
	assert.equal(badgeOf(n).color, "red");
	const row = once(<ListRow node={n} selected={false} width={78} />);
	noLineWiderThan(row, 78);
	assert.match(row, /312 behind · 2 conflicts/, "the parent's name goes when it doesn't fit");
	assert.match(rows(detailRows(e, 80)), /merging main conflicts in 2 files: users\.go, cart\.tsx/);

	w.farBehind = { behind: 312, limit: 100 };
	w.outOfStep = [];
	assert.deepEqual(badgeOf(nodeOf(snap, "cart")).texts, ["312 behind main", "312 behind"]);
	assert.match(rows(detailRows(entries(snap)[1]!, 80)), /far behind: 312 commits, and the merge only gets harder/);
});

// ── The tree ──────────────────────────────────────────────────────────

/** A worktree on parent, behind it by behind, for the tree's tests. */
function wt(branch: string, parent: string, behind: number, more: Partial<WorktreeState> = {}): WorktreeState {
	const at = new Date().toISOString();
	return {
		root: `/repo/${branch}`,
		dir: branch,
		branch,
		slot: 0,
		up: false,
		commit: "0123456ab",
		subject: branch,
		behind: 0,
		ahead: 1,
		stack: parent === "main" ? undefined : [parent, branch],
		outOfStep: [],
		services: [],
		memory: 0,
		problems: [],
		update: { parent, behind, note: behind ? `${parent} would be merged in` : "up to date" },
		plannedAt: at,
		gitAt: at,
		...more,
	};
}

function snapOf(worktrees: WorktreeState[], mainBehind = 0): Snapshot {
	const s = fixture();
	s.main!.behind = mainBehind;
	s.worktrees = worktrees;
	return s;
}

/** The whole dashboard once it has read the snapshot. */
async function framed(snap: Snapshot, columns = 80, rows = 24): Promise<string> {
	const r = render(<App backend={new FakeBackend(snap)} size={{ columns, rows }} />);
	await tick();
	const f = plain(r.lastFrame());
	r.unmount();
	return f;
}

const drawn = (snap: Snapshot, width = 78) =>
	tree(entries(snap)).map((n) => once(<ListRow node={n} selected={false} width={width} />));

test("a stack four deep nests each branch under its parent", () => {
	const snap = snapOf([
		wt("santiago/fla-712-the-fourth-layer-of-the-stack-with-a-long-title", "santiago/fla-711-third", 0),
		wt("santiago/fla-709-an-advisor-splits-a-sold-policy", "main", 0),
		wt("santiago/fla-711-third", "santiago/fla-710-second", 0),
		wt("santiago/fla-710-second", "santiago/fla-709-an-advisor-splits-a-sold-policy", 0),
	]);
	const nodes = tree(entries(snap));
	assert.deepEqual(
		nodes.map((n) => `${n.depth}:${n.lead}`),
		["0:", "1:└─", "2:  └─", "3:    └─", "4:      └─"],
	);
	const lines = drawn(snap);
	lines.forEach((l) => noLineWiderThan(l, 78));
	assert.match(lines[1]!, /^ └─▣ fla-709 an advisor splits a so… down\s+in step$/);
	assert.match(lines[4]!, /^ {7}└─▣ fla-712 the fourth layer…\s+down\s+in step$/, "the title gives way, not the columns");
	assert.match(lines[4]!, /…/);
	for (const n of nodes) assert.ok(n.stale.kind === "current" || n.e.kind === "main");
});

test("siblings share their parent's line, sorted by name", () => {
	const snap = snapOf([wt("zeta", "main", 0), wt("beta", "alpha", 0), wt("alpha", "main", 0), wt("gamma", "alpha", 0)]);
	const lines = drawn(snap);
	assert.match(lines[1]!, /^ ├─▣ alpha/);
	assert.match(lines[2]!, /^ │ ├─▣ beta/);
	assert.match(lines[3]!, /^ │ └─▣ gamma/);
	assert.match(lines[4]!, /^ └─▣ zeta/);
	// Starting a stack doesn't move its row.
	snap.worktrees[0]!.up = true;
	assert.match(drawn(snap)[4]!, /zeta/);
});

test("a stale parent makes its in-step child out of date too, and says where it starts", async () => {
	const snap = snapOf([wt("a", "main", 12), wt("b", "a", 0), wt("c", "b", 0)]);
	const nodes = tree(entries(snap));
	assert.deepEqual(nodes.map((n) => n.stale.kind), ["none", "behind", "inherits", "inherits"]);
	const lines = drawn(snap);
	assert.match(lines[1]!, /└─▣ a\s+down\s+12 behind main$/);
	assert.match(lines[2]!, /└─▣ b\s+down\s+↑ a is behind$/);
	assert.match(lines[3]!, /└─▣ c\s+down\s+↑ a is behind$/, "the start of the stale part, not the parent");
	assert.equal(badgeOf(nodes[1]!).bold, true, "where it goes stale is drawn strong");
	assert.equal(badgeOf(nodes[2]!).dim, true, "what inherits it, weaker");
	const f = await framed(snap);
	assert.match(f, /0 of 3 worktrees up · 3 out of date/);
});

test("an in-step parent with a stale child: only the child is out of date", async () => {
	const snap = snapOf([wt("a", "main", 0), wt("b", "a", 4, { update: { parent: "a", behind: 4, note: "CONFLICTS", hand: true, conflicts: ["x.go"] } }), wt("c", "b", 0)]);
	const nodes = tree(entries(snap));
	assert.deepEqual(nodes.map((n) => n.stale.kind), ["none", "current", "behind", "inherits"]);
	const lines = drawn(snap);
	assert.match(lines[1]!, /└─▣ a\s+down\s+in step$/);
	assert.match(lines[2]!, /└─▣ b\s+down\s+4 behind a · 1 conflict$/);
	assert.match(lines[3]!, /└─▣ c\s+down\s+↑ b is behind$/);
	const f = await framed(snap);
	assert.match(f, /0 of 3 worktrees up · 2 out of date/);
});

test("a whole chain in step says so, and main behind its upstream is said once, at the top", async () => {
	const snap = snapOf([wt("a", "main", 0), wt("b", "a", 0)], 10);
	const f = await framed(snap);
	assert.match(f, /main up, 10 behind upstream \(U\) · 0 of 2 worktrees up · all in step/);
	assert.match(f, /└─▣ a\s+down\s+in step/);
	assert.match(f, /└─▣ b\s+down\s+in step/);
	assert.equal(f.match(/behind upstream/g)!.length, 2, "the header and main's row, not the worktrees'");
});

test("a deep, wide tree fits 80 columns without a wrap", async () => {
	const long = "santiago/fla-7%d-a-customer-approves-their-quote-from-the-portal-and-signs";
	const snap = snapOf([
		wt(long.replace("%d", "01"), "main", 69, { update: { parent: "main", behind: 69, note: "CONFLICTS", hand: true, conflicts: ["a", "b", "c", "d", "e"] } }),
		wt(long.replace("%d", "02"), long.replace("%d", "01"), 7),
		wt(long.replace("%d", "03"), long.replace("%d", "02"), 0),
		wt(long.replace("%d", "04"), long.replace("%d", "03"), 0),
		wt(long.replace("%d", "05"), long.replace("%d", "04"), 0),
		wt(long.replace("%d", "06"), long.replace("%d", "01"), 0),
		wt("santiago/supabase-native-local-with-a-very-long-name", "main", 32),
	]);
	for (const [w, h] of [[80, 24], [120, 30], [200, 40]] as const) {
		const r = render(<App backend={new FakeBackend(snap)} size={{ columns: w, rows: h }} />);
		await tick();
		const f = plain(r.lastFrame());
		noLineWiderThan(f, w);
		assert.equal(f.split("\n").length, h - 1);
		r.unmount();
	}
	const lines = drawn(snap);
	lines.forEach((l) => noLineWiderThan(l, 78));
	assert.match(lines[1]!, /├─▣ fla-701 .*69 behind · 5 conflicts$/);
	assert.match(lines[5]!, /^ │ │ {5}└─▣ fla-705 /);
	assert.match(lines[5]!, /↑ fla-701 is behind$/);
	assert.match(lines[2]!, /7 behind fla-701$/, "behind its own parent too, after the one above it");
	assert.equal(badgeOf(tree(entries(snap))[2]!).bold, false, "not where the chain goes stale");
	// The narrowest list (a wide terminal's left column): still one line each.
	drawn(snap, 58).forEach((l) => noLineWiderThan(l, 58));
});

test("m on a branch in step with its parent says which branch to update first", async () => {
	const snap = snapOf([wt("a", "main", 12), wt("b", "a", 0)]);
	snap.current = "/repo/b";
	const backend = new FakeBackend(snap);
	const r = render(<App backend={backend} size={{ columns: 80, rows: 30 }} />);
	await tick();
	r.stdin.write("m");
	await tick();
	assert.match(plain(r.lastFrame()), /b has everything a has; a is behind: update it first/);
	assert.equal(backend.runs.length, 0);
	r.unmount();
});

// ── Stack branches with no checkout ───────────────────────────────────

/** The PR stack main <- schema <- core <- web with only web checked out.
 *  web merged main straight in; schema and core are 10 behind it. */
function ring(): Snapshot {
	const at = new Date().toISOString();
	const b = (name: string, parent: string, behind: number, stack: string[]): Branch => ({
		branch: `santiago/scrm-264-app-ring-${name}`,
		commit: "0123456ab",
		subject: name,
		behind: 10,
		ahead: stack.length,
		stack: stack.map((s) => `santiago/scrm-264-app-ring-${s}`),
		source: "gh-stack",
		update: { parent, behind, action: behind ? "merge" : undefined, note: behind ? `${parent} would be merged in (${behind} commits), without a checkout` : "up to date" },
		gitAt: at,
	});
	const s = snapOf([
		wt("santiago/scrm-264-app-ring-web", "santiago/scrm-264-app-ring-core", 0, {
			stack: ["schema", "core", "web"].map((x) => `santiago/scrm-264-app-ring-${x}`),
		}),
	]);
	s.branches = [b("core", "santiago/scrm-264-app-ring-schema", 0, ["schema", "core"]), b("schema", "main", 10, ["schema"])];
	return s;
}

test("a stack's branches with no checkout are rows in their place in the chain", async () => {
	const snap = ring();
	const nodes = tree(entries(snap));
	assert.deepEqual(
		nodes.map((n) => `${n.depth}:${n.e.kind}:${n.stale.kind}`),
		["0:main:none", "1:branch:behind", "2:branch:inherits", "3:worktree:inherits"],
	);
	const lines = drawn(snap);
	lines.forEach((l) => noLineWiderThan(l, 78));
	assert.match(lines[1]!, /^ └─⎇ scrm-264 app ring schema\s+branch only\s+10 behind main$/);
	assert.match(lines[2]!, /^ {3}└─⎇ scrm-264 app ring core\s+branch only\s+↑ schema is behind$/);
	assert.match(lines[3]!, /^ {5}└─▣ scrm-264 app ring web\s+down\s+↑ schema is behind$/, "web merged main, but its stack didn't");
	assert.equal(badgeOf(nodes[1]!).bold, true, "where the chain goes stale is drawn strong");
	const f = await framed(snap);
	assert.match(f, /0 of 1 worktree up · 3 out of date/, "the header counts worktrees; what is out of date counts every branch");
	noLineWiderThan(f, 80);
	// Narrow (a wide terminal's left column): the ticket, then what tells
	// the branches apart, rather than three cut-off titles alike.
	const wide = await framed(snap, 120, 30);
	assert.match(wide, /└─⎇ scrm-264 schema +branch only/);
	assert.match(wide, /└─⎇ scrm-264 core +branch only/);
	assert.match(wide, /└─▣ scrm-264 w(eb|…) +down/);
});

test("a branch with no checkout: its detail, and only m in the footer", () => {
	const es = entries(ring());
	const schema = es.find((e) => e.branch.endsWith("schema"))!;
	const core = es.find((e) => e.branch.endsWith("core"))!;
	assert.equal(stateOf(schema).word, "branch only");
	assert.deepEqual(footerKeys(schema), [["m", "merge main"]]);
	assert.deepEqual(footerKeys(core), [], "in step with schema: nothing to merge yet");
	const nodes = tree(es);
	const node = nodes.find((n) => n.e === core)!;
	const text = rows(detailRows(core, 80, { node, staleOf: (e) => nodes.find((n) => n.e === e)!.stale }));
	assert.match(text, /santiago\/scrm-264-app-ring-core · branch only: no worktree, no files on disk/);
	assert.match(text, /vs schema +in step/);
	assert.match(text, /main → schema \(10 behind\) → core/);
	assert.match(text, /schema is behind main: update it first/);
	assert.match(text, /◆ Runs here {2}nothing: no worktree has it checked out/);
	assert.doesNotMatch(text, /From main/);
	assert.match(once(<>{problemRows(core, 60).map((s, i) => <Row key={i} segs={s} width={60} />)}</>), /nothing runs it: branch only, no worktree/);
});

test("m on a branch with no checkout merges without one, from main's checkout; other keys say why not", async () => {
	const snap = ring();
	snap.current = snap.main!.root;
	const backend = new FakeBackend(snap);
	backend.results["update --branch santiago/scrm-264-app-ring-schema --dry-run"] = {
		code: 0,
		output: "worktrees\n  (no checkout)  scrm-264-app-ring-schema  main would be merged in (10 commits), without a checkout\n",
	};
	const r = render(<App backend={backend} size={{ columns: 100, rows: 30 }} />);
	await tick();
	r.stdin.write(DOWN);
	await tick();
	assert.match(plain(r.lastFrame()), /▌└─⎇ scrm-264 app ring schema/);
	r.stdin.write("u");
	await tick();
	assert.match(plain(r.lastFrame()), /is a branch only, with no worktree: nothing runs it/);
	assert.equal(backend.runs.length, 0);
	r.stdin.write("m");
	await tick();
	assert.match(plain(r.lastFrame()), /Merge main into schema, without a checkout\? \(hydra update --branch\)/);
	r.stdin.write("y");
	await tick();
	assert.deepEqual(backend.terminals[0]!.argv, ["hydra", "update", "--branch", "santiago/scrm-264-app-ring-schema"]);
	assert.equal(backend.terminals[0]!.cwd, "/repo/main");
	r.unmount();
});

// ── Pull requests ─────────────────────────────────────────────────────

/** The ring stack with its PRs: schema merged on GitHub, core a draft,
 *  web open with its checks passing, and gh read a minute ago. */
function ringPRs(): Snapshot {
	const s = ring();
	const url = (n: number) => `https://github.com/o/r/pull/${n}`;
	s.branches!.find((b) => b.branch.endsWith("schema"))!.pr = { number: 10264, state: "merged", url: url(10264), base: "main" };
	s.branches!.find((b) => b.branch.endsWith("core"))!.pr = { number: 10265, state: "draft", checks: "pending", url: url(10265) };
	s.worktrees[0]!.pr = { number: 10266, state: "open", review: "review required", checks: "passing", url: url(10266) };
	s.prs = { at: new Date(Date.now() - 60000).toISOString() };
	return s;
}

test("each row has its PR in a few columns, and a merged one says nothing is left to do", async () => {
	const snap = ringPRs();
	const nodes = tree(entries(snap));
	const lines = nodes.map((n) => once(<ListRow node={n} selected={false} width={78} prW={13} />));
	lines.forEach((l) => noLineWiderThan(l, 78));
	assert.match(lines[1]!, /⎇ \S.*branch only +#10264 merged +PR merged: nothing to do$/);
	assert.match(lines[2]!, /branch only +#10265 draft +↑ schema is behind$/);
	assert.match(lines[3]!, /down +#10266 ✓ +↑ schema is behind$/);
	// The whole dashboard, at 80 columns: the PR column fits beside the
	// tree and the staleness, the header counts the merged PR, no wraps.
	const f = await framed(snap, 80, 30);
	noLineWiderThan(f, 80);
	assert.match(f, /#10264 merged/);
	assert.match(f, /#10266 ✓/);
	assert.match(f, /3 out of date · 1 on a merged PR/);
	// Too narrow for it: the PR column goes first; the marker stays.
	const narrow = await framed(snap, 60, 30);
	noLineWiderThan(narrow, 60);
	assert.doesNotMatch(narrow, /#10266/);
	assert.match(narrow, /PR merged/);
});

test("the detail says, in order: the PR, the chain, what runs here; a branch with no checkout names who runs it", () => {
	const snap = ringPRs();
	const nodes = tree(entries(snap));
	const staleOf = (e: Entry) => nodes.find((n) => n.e === e)!.stale;
	const web = nodes[3]!;
	const text = rows(detailRows(web.e, 80, { node: web, staleOf, prs: snap.prs }));
	assert.match(text, /◎ PR {2}#10266 open · review required · checks passing/);
	assert.match(text, /https:\/\/github\.com\/o\/r\/pull\/10266 {2}p opens it/);
	assert.match(text, /main → schema \(10 behind\) → core → web/);
	assert.ok(text.indexOf("◎ PR") < text.indexOf("main → schema") && text.indexOf("main → schema") < text.indexOf("◆ Runs here"));

	const schema = nodes[1]!;
	const s = rows(detailRows(schema.e, 80, { node: schema, staleOf, prs: snap.prs, via: [web.e] }));
	assert.match(s, /◎ PR {2}#10264 merged/);
	assert.match(s, /PR merged: nothing left to do here/);
	assert.match(s, /◆ Runs here {2}nothing: no worktree has it checked out/);
	assert.match(s, /web, stacked on it, runs its code/);
	assert.deepEqual(footerKeys(schema.e), [["p", "PR"]], "merged: nothing to merge into it, only its PR to open");
});

test("p opens the PR; without gh there are no badges and the help says why", async () => {
	const snap = ringPRs();
	const backend = new FakeBackend(snap);
	const r = render(<App backend={backend} size={{ columns: 100, rows: 40 }} />);
	await tick();
	r.stdin.write("p");
	await tick();
	assert.match(plain(r.lastFrame()), /main has no PR/);
	r.stdin.write("jjj"); // web
	await tick();
	r.stdin.write("p");
	await tick();
	assert.deepEqual(backend.browsed, ["https://github.com/o/r/pull/10266"]);
	r.stdin.write("?");
	await tick();
	assert.match(plain(r.lastFrame()), /PR status from gh, read 1 min ago/);
	assert.match(plain(r.lastFrame()), /p +open its pull request in the browser/);
	r.unmount();

	const none = ring();
	none.prs = { error: "gh isn't installed: PR status needs GitHub's CLI (gh), and is optional" };
	const f = await framed(none, 80, 30);
	assert.doesNotMatch(f, /#102|◎ PR|PR merged|gh/, "no badges, no error");
	const r2 = render(<App backend={new FakeBackend(none)} size={{ columns: 100, rows: 40 }} />);
	await tick();
	r2.stdin.write("?");
	await tick();
	assert.match(plain(r2.lastFrame()), /no PR status: gh isn't installed/);
	r2.stdin.write("q");
	r2.unmount();
});

/** The fixture with a tunnel cart holds. */
function tunnelled(): Snapshot {
	const s = fixture();
	s.tunnels = [{ name: "hooks", url: "https://hooks.example.com", to: "api", port: 8079, holder: "/repo/cart", target: "cart:8180" }];
	s.worktrees.find((w) => w.dir === "cart")!.tunnel = ["hooks"];
	return s;
}

function tunnelApp(width = 80, rows = 30) {
	const backend = new FakeBackend(tunnelled());
	const r = render(<App backend={backend} size={{ columns: width, rows }} />);
	return { backend, ...r, frame: () => plain(r.lastFrame()) };
}

test("the tunnel: its holder in the header and its row, a line in each detail, t in the footer; nothing wraps", async () => {
	for (const width of [80, 160]) {
		const { frame, unmount } = tunnelApp(width, 40);
		await tick();
		const f = frame();
		noLineWiderThan(f, width);
		assert.match(f, /├─✗ ⇄ cart-checkout/, "the holder's row");
		assert.match(f, /⇄ Tunnel {2}hooks https:\/\/hooks\.example\.com comes here/);
		assert.match(f, /t tunnel/);
		if (width >= 120) assert.match(f, /⇄ tunnel: cart-checkout/);
		unmount();
	}
	const main = entries(tunnelled()).find((e) => e.kind === "main")!;
	const f = rows(detailRows(main, 76, { tunnels: [{ t: tunnelled().tunnels![0]!, holder: "cart-checkout" }] }));
	assert.match(f, /⇄ Tunnel {2}hooks https:\/\/hooks\.example\.com goes to cart-checkout/);
	assert.match(f, /t gives it back to main/);
});

test("t asks first, then takes the tunnel to the chosen worktree; in its holder it gives it back", async () => {
	const { backend, stdin, frame, unmount } = tunnelApp(80, 40);
	await tick();
	stdin.write("t"); // on cart, which holds it: back to main
	await tick();
	assert.match(frame(), /Give tunnel hooks \(https:\/\/hooks\.example\.com\) back to main\?/);
	stdin.write("n");
	await tick();
	stdin.write(DOWN);
	await tick();
	stdin.write("t");
	await tick();
	assert.match(frame(), /Move tunnel hooks \(https:\/\/hooks\.example\.com\) from cart-checkout to/);
	assert.match(frame(), /Its webhooks will reach main's api, tagged as/);
	assert.equal(backend.runs.length, 0, "nothing runs before the yes");
	stdin.write("y");
	await tick();
	const taken = backend.runs.at(-1)!;
	assert.deepEqual(taken.args, ["tunnel", "take", "hooks"]);
	assert.notEqual(taken.cwd, "/repo/cart");
	assert.notEqual(taken.cwd, "/repo/main");
	unmount();
});

test("why a service runs stays on one line: long paths give up their middle", () => {
	const why = "changed: apps/web/app/(auth)/[org-slug]/(default)/agent/components/agent-workspace/activity/activity.constants.ts";
	const got = fitPaths(why, 60);
	assert.ok(Array.from(got).length <= 60, got);
	assert.match(got, /^changed: apps\/.*…\/activity\.constants\.ts$/);
	assert.equal(fitPaths("changed: apps/api/orders.go", 60), "changed: apps/api/orders.go");
	assert.equal(fitPaths("needed by api", 60), "needed by api");
});

test("where a service changed: the folder its files share, and up to three things in it", () => {
	assert.deepEqual(changedIn(["services/core/cmd/api/main.go"]), { dir: "services/core/cmd/api/", items: [{ name: "main.go", folder: false }] });
	assert.deepEqual(changedIn(["services/core/cmd/api/main.go", "services/core/internal/x.go", "services/core/go.mod"]), {
		dir: "services/core/",
		items: [
			{ name: "cmd", folder: true },
			{ name: "go.mod", folder: false },
			{ name: "internal", folder: true },
		],
	});
	const rows = (r: ReturnType<typeof changedRows>) => r.map((segs) => segs.map((x) => x.text).join("")).join("\n");
	assert.equal(rows(changedRows("changed: services/core/cmd/api/main.go", ["services/core/cmd/api/main.go"], 70)), "changed: services/core/cmd/api/ 📄 main.go");
	assert.equal(
		rows(changedRows("changed in core: a/b/c.go", ["a/b/c.go", "a/d/e.go", "a/f.go", "a/g/h.go", "a/i/j.go"], 70)),
		"changed in core: a/\n  📁 b/\n  📁 d/\n  📄 f.go\n  … 2 more",
	);
});
