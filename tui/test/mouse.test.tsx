import { test } from "node:test";
import assert from "node:assert/strict";
import { PassThrough } from "node:stream";
import React from "react";
import { render } from "ink-testing-library";
import App from "../src/App.js";
import { eventOf, mouseSource, takeMouse, type MouseEvent } from "../src/mouse.js";
import { watchTheme } from "../src/theme.js";
import { FakeBackend, plain, tick } from "./fake.js";
import { fixture } from "./fixture.js";

test("mouse reports are taken out of the input; one cut off is held for the next read", () => {
	const got = takeMouse("j\x1b[<0;5;3Mk\x1b[<0;5;3m\x1b[<65;10;20M\x1b[<32;7;4M");
	assert.equal(got.rest, "jk");
	assert.deepEqual(got.events, [
		{ kind: "press", button: 0, x: 4, y: 2, mod: false },
		{ kind: "release", button: 0, x: 4, y: 2, mod: false },
		{ kind: "wheel", button: 0, x: 9, y: 19, dir: 1, mod: false },
		{ kind: "drag", button: 0, x: 6, y: 3, mod: false },
	]);
	const a = takeMouse("q\x1b[<64;3");
	assert.deepEqual([a.rest, a.held, a.events.length], ["q", "\x1b[<64;3", 0]);
	assert.deepEqual(takeMouse(a.held + ";9M").events, [{ kind: "wheel", button: 0, x: 2, y: 8, dir: -1, mod: false }]);
	assert.equal(takeMouse("\x1b[B").rest, "\x1b[B", "an arrow key is a key");
	assert.equal(eventOf(66, 1, 1, false), undefined, "the wheel sideways does nothing");
	assert.equal(eventOf(4, 1, 1, false)?.mod, true, "shift is said, so a click with it is left to the terminal");
});

test("the terminal's filter hands mouse reports to the dashboard and keys to Ink", async () => {
	const input = new PassThrough() as unknown as NodeJS.ReadStream & PassThrough;
	Object.assign(input, { isTTY: true, isRaw: false, setRawMode: () => input, ref: () => input, unref: () => input });
	const events: MouseEvent[] = [];
	const w = watchTheme(input, () => {}, () => {}, { waitMs: 10, onMouse: (e) => events.push(e) });
	const keys: string[] = [];
	w.stdin.setEncoding("utf8");
	w.stdin.on("data", (d: string) => keys.push(d));
	input.write("j\x1b[<0;2;3M");
	input.write("\x1b[<0;2;3mk");
	await tick();
	w.stop();
	assert.deepEqual(keys.join(""), "jk");
	assert.deepEqual(events.map((e) => e.kind), ["press", "release"]);
});

/** The dashboard with a mouse to click. */
function app(width = 80, rows = 30) {
	const backend = new FakeBackend(fixture());
	const m = mouseSource();
	const r = render(<App backend={backend} size={{ columns: width, rows }} mouse={m.source} />);
	const frame = () => plain(r.lastFrame());
	/** The row and column of text on the screen. */
	const at = (text: string, from = 0): { x: number; y: number } => {
		const lines = frame().split("\n");
		for (let y = from; y < lines.length; y++) {
			const x = lines[y]!.indexOf(text);
			if (x >= 0) return { x: Array.from(lines[y]!.slice(0, x)).length, y };
		}
		throw new Error(`no ${JSON.stringify(text)} on the screen:\n${frame()}`);
	};
	const click = async (p: { x: number; y: number }) => {
		m.emit({ kind: "press", button: 0, ...p });
		m.emit({ kind: "release", button: 0, ...p });
		await tick();
	};
	const wheel = async (p: { x: number; y: number }, dir: number) => {
		m.emit({ kind: "wheel", button: 0, dir, ...p });
		await tick();
	};
	return { backend, ...r, frame, at, click, wheel, emit: m.emit };
}

test("a click on a checkout chooses it", async () => {
	const a = app();
	await tick();
	await a.click(a.at("clash"));
	assert.match(a.frame(), /╭─ clash /);
	a.unmount();
});

test("a click on a key in the footer presses it, and on y in a confirmation says yes", async () => {
	const a = app();
	await tick();
	const footer = a.frame().split("\n").length - 1;
	await a.click(a.at("d down", footer));
	assert.match(a.frame(), /Stop cart-checkout's stack\? \(hydra down\)/);
	await a.click(a.at("yes, do it"));
	assert.deepEqual(a.backend.runs.at(-1), { args: ["down"], cwd: "/repo/cart" });
	a.unmount();
});

test("the wheel scrolls the detail, and moves the choice in the list", async () => {
	const a = app(80, 24);
	await tick();
	assert.match(a.frame(), /branch {2}santiago\/cart-checkout/);
	const detail = a.at("╭─ cart-checkout");
	await a.wheel({ x: 10, y: detail.y + 2 }, 1);
	assert.doesNotMatch(a.frame(), /branch {2}santiago\/cart-checkout/, "scrolled past its first lines");
	await a.wheel({ x: 10, y: detail.y + 2 }, -1);
	assert.match(a.frame(), /branch {2}santiago\/cart-checkout/, "and back");
	await a.wheel({ x: 10, y: 3 }, 1);
	assert.match(a.frame(), /╭─ more /, "the wheel over the list moves the choice");
	a.unmount();
});

test("dragging the border between the list and the detail resizes them", async () => {
	const a = app(80, 40);
	await tick();
	const border = a.at("╰──").y;
	const before = a.at("╭─ cart-checkout").y;
	a.emit({ kind: "press", button: 0, x: 20, y: border });
	a.emit({ kind: "drag", button: 0, x: 20, y: border + 4 });
	a.emit({ kind: "release", button: 0, x: 20, y: border + 4 });
	await tick();
	assert.equal(a.at("╭─ cart-checkout").y, before + 4);
	a.unmount();
});

test("wide: dragging the border between the columns makes the list wider", async () => {
	const a = app(160, 40);
	await tick();
	const top = a.frame().split("\n")[1]!;
	const edge = Array.from(top).indexOf("╮");
	a.emit({ kind: "press", button: 0, x: edge, y: 5 });
	a.emit({ kind: "drag", button: 0, x: edge + 10, y: 5 });
	a.emit({ kind: "release", button: 0, x: edge + 10, y: 5 });
	await tick();
	assert.equal(Array.from(a.frame().split("\n")[1]!).indexOf("╮"), edge + 10);
	a.unmount();
});

test("a click on an address opens it; on a branch in the chain, chooses it", async () => {
	const a = app(100, 44);
	await tick();
	await a.click(a.at("http://cart.localhost:3900", a.at("╭─ cart-checkout").y));
	assert.deepEqual(a.backend.browsed, ["http://cart.localhost:3900"]);
	await a.click(a.at("more"));
	assert.match(a.frame(), /╭─ more /);
	await a.click(a.at("cart-checkout", a.at("⇅ Chain").y + 1));
	assert.match(a.frame(), /╭─ cart-checkout /);
	a.unmount();
});
