import { test } from "node:test";
import assert from "node:assert/strict";
import { PassThrough } from "node:stream";
import { ASK, ASK_BACKGROUND, REPORT_OFF, REPORT_ON, takeReplies, themeFor, watchTheme, type Theme } from "../src/theme.js";
import { tick } from "./fake.js";

test("background answers and scheme reports are taken out of the keys", () => {
	const got = takeReplies("j\x1b]11;rgb:ffff/ffff/ffff\x07k\x1b[?997;1n\x1b]11;rgb:2e/34/40\x1b\\q");
	assert.equal(got.rest, "jkq");
	assert.equal(got.held, "");
	assert.deepEqual(
		got.replies.map((r) => `${r.from}:${r.mode}`),
		["background:light", "scheme:dark", "background:dark"],
	);
	assert.deepEqual(takeReplies("\x1b[?997;2n").replies, [{ mode: "light", from: "scheme" }]);
	assert.deepEqual(takeReplies("\x1b]11;rgba:eeee/f1f1/f5f5/ffff\x07").replies, [{ mode: "light", from: "background" }]);
});

test("a reply cut off at the end of a read is held for the next one; keys are not", () => {
	const a = takeReplies("x\x1b]11;rgb:30/34");
	assert.deepEqual([a.rest, a.held, a.replies.length], ["x", "\x1b]11;rgb:30/34", 0]);
	const b = takeReplies(a.held + "/46\x07y");
	assert.deepEqual([b.rest, b.held, b.replies[0]?.mode], ["y", "", "dark"]);
	assert.equal(takeReplies("\x1b[?99").held, "\x1b[?99");
	// The escape key and an arrow key go straight through.
	assert.deepEqual(takeReplies("\x1b"), { rest: "\x1b", held: "", replies: [] });
	assert.deepEqual(takeReplies("\x1b[B"), { rest: "\x1b[B", held: "", replies: [] });
});

test("each theme has a selection that reads, and an unknown one has none", () => {
	assert.deepEqual(themeFor("light"), { mode: "light", selectionBg: "#bfdbfe", yellow: "#8a5a00" });
	assert.deepEqual(themeFor("dark"), { mode: "dark", selectionBg: "#264b78", yellow: "yellow" });
	assert.deepEqual(themeFor(), { yellow: "yellow" });
});

/** A terminal: what the dashboard wrote, and a stdin to answer on. */
function terminal() {
	const input = new PassThrough() as unknown as NodeJS.ReadStream & PassThrough;
	let raw = false;
	Object.assign(input, { isTTY: true, isRaw: false, setRawMode: (on: boolean) => ((raw = on), input), ref: () => input, unref: () => input });
	const wrote: string[] = [];
	const themes: Theme[] = [];
	return { input, wrote, themes, raw: () => raw };
}

test("the theme follows the terminal while the dashboard runs, and keys still reach it", async () => {
	const t = terminal();
	const w = watchTheme(t.input, (s) => t.wrote.push(s), (th) => t.themes.push(th), { waitMs: 1000, everyMs: 20 });
	assert.equal(t.wrote[0], REPORT_ON + ASK);
	assert.ok(t.raw(), "stdin is raw before the answers come");
	const keys: string[] = [];
	w.stdin.setEncoding("utf8");
	w.stdin.on("data", (d: string) => keys.push(d));

	// A terminal without scheme reports: asked again and again.
	t.input.write("\x1b]11;rgb:ffff/ffff/ffff\x07");
	await w.ready;
	assert.equal(w.theme().mode, "light");
	await new Promise((r) => setTimeout(r, 70));
	assert.ok(t.wrote.filter((s) => s === ASK_BACKGROUND).length >= 2, "asks for the background again");
	t.input.write("j\x1b]11;rgb:2e2e/3434/4040\x07");
	await tick();
	assert.deepEqual(t.themes.map((th) => th.mode), ["light", "dark"]);
	assert.equal(keys.join(""), "j");

	// Once it reports changes, it isn't asked any more.
	t.input.write("\x1b[?997;2n");
	await tick();
	const asked = t.wrote.length;
	await new Promise((r) => setTimeout(r, 70));
	assert.equal(t.wrote.length, asked);
	assert.deepEqual(t.themes.map((th) => th.mode), ["light", "dark", "light"]);

	w.stop();
	assert.equal(t.wrote.at(-1), REPORT_OFF);
});

test("a terminal that never answers leaves the theme unknown", async () => {
	const t = terminal();
	const w = watchTheme(t.input, (s) => t.wrote.push(s), (th) => t.themes.push(th), { waitMs: 20, everyMs: 10 });
	await w.ready;
	await new Promise((r) => setTimeout(r, 40));
	assert.deepEqual(w.theme(), { yellow: "yellow" });
	assert.equal(t.wrote.length, 1, "asks once, and not again");
	w.stop();
});
