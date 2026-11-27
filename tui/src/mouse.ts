// The mouse. The dashboard asks the terminal to report presses, releases,
// drags and the wheel (button-event tracking, mode 1002) in SGR form (mode
// 1006, which has no limit on columns): ESC [ < button ; column ; row M for
// a press or a move, m for a release. The reports arrive on stdin among the
// keys, so they are taken out of it before Ink reads it (theme.ts), and
// handed to the App as events.

/** Turns the reports on and off. */
export const MOUSE_ON = "\x1b[?1002h\x1b[?1006h";
export const MOUSE_OFF = "\x1b[?1006l\x1b[?1002l";

export interface MouseEvent {
	/** press and release of a button, a move with one held, or a turn of the wheel */
	kind: "press" | "release" | "drag" | "wheel";
	/** 0 left, 1 middle, 2 right */
	button: number;
	/** the cell, from 0 at the top left */
	x: number;
	y: number;
	/** the wheel: -1 up, 1 down */
	dir?: number;
	/** shift, alt or ctrl held */
	mod?: boolean;
}

const REPORT = /\x1b\[<(\d+);(\d+);(\d+)([Mm])/g;
// A report cut off at the end of a read.
const PARTIAL = /\x1b\[<[\d;]*$/;

/** What a report says. */
export function eventOf(code: number, col: number, row: number, release: boolean): MouseEvent | undefined {
	const x = col - 1;
	const y = row - 1;
	const mod = (code & 28) !== 0;
	if (code & 64) {
		const b = code & 3;
		if (b > 1) return undefined; // the wheel sideways
		return { kind: "wheel", button: 0, x, y, dir: b === 0 ? -1 : 1, mod };
	}
	const button = code & 3;
	if (release) return { kind: "release", button, x, y, mod };
	if (code & 32) return button === 3 ? undefined : { kind: "drag", button, x, y, mod };
	if (button === 3) return { kind: "release", button: 0, x, y, mod };
	return { kind: "press", button, x, y, mod };
}

/**
 * Takes the mouse reports out of what the terminal sent. held is the start
 * of a report the read cut off: it goes in front of the next read.
 */
export function takeMouse(input: string): { rest: string; held: string; events: MouseEvent[] } {
	const events: MouseEvent[] = [];
	let rest = input.replace(REPORT, (_m, b: string, c: string, r: string, kind: string) => {
		const e = eventOf(Number(b), Number(c), Number(r), kind === "m");
		if (e) events.push(e);
		return "";
	});
	const cut = PARTIAL.exec(rest);
	const held = cut ? cut[0] : "";
	if (cut) rest = rest.slice(0, cut.index);
	return { rest, held, events };
}

/** Where the App listens for the mouse: subscribe returns the unsubscribe. */
export type MouseSource = (listener: (e: MouseEvent) => void) => () => void;

/** A source to emit into: the terminal's filter, or a test. */
export function mouseSource(): { source: MouseSource; emit: (e: MouseEvent) => void } {
	const listeners = new Set<(e: MouseEvent) => void>();
	return {
		source: (l) => {
			listeners.add(l);
			return () => listeners.delete(l);
		},
		emit: (e) => listeners.forEach((l) => l(e)),
	};
}
