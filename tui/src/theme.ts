// The terminal's theme, light or dark, and the few colours that depend on it.
//
// Text uses the terminal's own palette (Ink's named colours), which its theme
// already tunes for its background. Two things can't: the selected row's
// background, a colour of its own, and yellow, which most light palettes
// make too pale to read on white. Both are picked here per theme.
//
// The theme is asked for at start and kept up to date while the dashboard
// runs, because a terminal that follows the system appearance switches
// under it: terminals that report changes (mode 2031: Ghostty, kitty, tmux
// 3.6+) say so, and the others are asked for their background again every
// few seconds. The replies arrive on stdin, among the keys, so they are
// taken out of it before Ink reads it, with the mouse's reports (mouse.ts).

import { PassThrough } from "node:stream";
import { takeMouse, type MouseEvent } from "./mouse.js";

export type Mode = "light" | "dark";

export interface Theme {
	/** undefined until the terminal says which */
	mode?: Mode;
	/** the selected row's background; none when the theme isn't known */
	selectionBg?: string;
	/** what the colour "yellow" is drawn in */
	yellow: string;
}

/**
 * The colours for a mode. Both backgrounds keep the terminal's default text
 * readable (4.5:1 or more on common palettes) and stand out from the
 * terminal's own background; the light theme's yellow is a dark amber that
 * reads on white, on Solarized's cream and on the selection.
 */
export function themeFor(mode?: Mode): Theme {
	if (mode === "light") return { mode, selectionBg: "#bfdbfe", yellow: "#8a5a00" };
	if (mode === "dark") return { mode, selectionBg: "#264b78", yellow: "yellow" };
	return { yellow: "yellow" };
}

/** Asks for the background colour (OSC 11) and the colour scheme (CSI ? 996 n). */
export const ASK = "\x1b]11;?\x07\x1b[?996n";
/** Asks for the background colour alone, for terminals that don't report changes. */
export const ASK_BACKGROUND = "\x1b]11;?\x07";
/** Turns reports of colour scheme changes on and off (mode 2031). */
export const REPORT_ON = "\x1b[?2031h";
export const REPORT_OFF = "\x1b[?2031l";

export interface Reply {
	mode: Mode;
	/** "background": an OSC 11 answer; "scheme": a colour scheme report */
	from: "background" | "scheme";
}

const BACKGROUND = /\x1b\]11;rgba?:([0-9a-f]+)\/([0-9a-f]+)\/([0-9a-f]+)(?:\/[0-9a-f]+)?(?:\x07|\x1b\\)/gi;
const SCHEME = /\x1b\[\?997;([12])n/g;
// The start of either reply, cut off at the end of a read. A lone ESC is the
// escape key, not a reply, so it is never held back.
const PARTIAL = /\x1b(?:\](?:1(?:1(?:;[^\x07\x1b]*\x1b?)?)?)?|\[\?(?:9(?:9(?:7(?:;[12]?)?)?)?)?)$/;

/** sRGB luminance of an OSC 11 answer's channels (2 or 4 hex digits each). */
function modeOf(r: string, g: string, b: string): Mode {
	const c = (h: string) => parseInt(h, 16) / (16 ** h.length - 1);
	return 0.2126 * c(r) + 0.7152 * c(g) + 0.0722 * c(b) > 0.5 ? "light" : "dark";
}

/**
 * Takes the theme replies out of what the terminal sent. held is the start
 * of a reply the read cut off: it goes in front of the next read.
 */
export function takeReplies(input: string): { rest: string; held: string; replies: Reply[] } {
	const found: Array<{ at: number; reply: Reply }> = [];
	let rest = input.replace(BACKGROUND, (_m, r: string, g: string, b: string, at: number) => {
		found.push({ at, reply: { mode: modeOf(r, g, b), from: "background" } });
		return "";
	});
	rest = rest.replace(SCHEME, (_m, n: string, at: number) => {
		found.push({ at, reply: { mode: n === "1" ? "dark" : "light", from: "scheme" } });
		return "";
	});
	const cut = PARTIAL.exec(rest);
	const held = cut ? cut[0] : "";
	if (cut) rest = rest.slice(0, cut.index);
	return { rest, held, replies: found.sort((a, b) => a.at - b.at).map((f) => f.reply) };
}

export interface ThemeWatch {
	/** stdin without the replies, for Ink */
	stdin: NodeJS.ReadStream;
	/** resolves at the first reply, or after waitMs without one */
	ready: Promise<void>;
	theme(): Theme;
	/** turns the reports off and stops asking */
	stop(): void;
}

/**
 * Asks the terminal for its theme and keeps asking (see the top of the
 * file). onChange runs when the mode changes. A terminal that never answers
 * leaves the mode unknown: the selection is then marked without a
 * background, which reads on any theme.
 */
export function watchTheme(
	input: NodeJS.ReadStream,
	write: (s: string) => void,
	onChange: (t: Theme) => void,
	{ waitMs = 200, everyMs = 2000, onMouse }: { waitMs?: number; everyMs?: number; onMouse?: (e: MouseEvent) => void } = {},
): ThemeWatch {
	let mode: Mode | undefined;
	let reports = false;
	let poll: NodeJS.Timeout | undefined;
	let held = "";
	let flush: NodeJS.Timeout | undefined;
	let answered: () => void = () => {};
	const ready = new Promise<void>((resolve) => {
		answered = resolve;
		setTimeout(resolve, waitMs);
	});

	const out = new PassThrough() as unknown as NodeJS.ReadStream & PassThrough;
	// What Ink needs of a terminal's stdin, passed on to the real one.
	Object.assign(out, {
		isTTY: input.isTTY,
		isRaw: input.isRaw,
		setRawMode(on: boolean) {
			input.setRawMode(on);
			out.isRaw = on;
			return out;
		},
		ref() {
			input.ref();
			return out;
		},
		unref() {
			input.unref();
			return out;
		},
	});

	const seen = (r: Reply) => {
		if (r.from === "scheme" && !reports) {
			// It reports changes, so asking again is no longer needed.
			reports = true;
			clearInterval(poll);
			poll = undefined;
		}
		if (r.from === "background" && !reports && !poll) {
			poll = setInterval(() => write(ASK_BACKGROUND), everyMs);
			poll.unref();
		}
		if (r.mode !== mode) {
			mode = r.mode;
			onChange(themeFor(mode));
		}
		answered();
	};

	input.setEncoding("utf8");
	input.on("data", (chunk: string) => {
		clearTimeout(flush);
		// The mouse's reports go too (mouse.ts).
		const mouse = takeMouse(held + chunk);
		const got = takeReplies(mouse.rest);
		held = got.held + mouse.held;
		got.replies.forEach(seen);
		if (onMouse) mouse.events.forEach(onMouse);
		if (got.rest) out.write(got.rest);
		// What looked like the start of a reply and wasn't (alt-] typed
		// alone) still reaches Ink, a moment late.
		if (held)
			flush = setTimeout(() => {
				out.write(held);
				held = "";
			}, 50);
	});

	// Raw already, so the answers neither echo nor wait for a newline.
	input.setRawMode(true);
	out.isRaw = true;
	write(REPORT_ON + ASK);
	return {
		stdin: out,
		ready,
		theme: () => themeFor(mode),
		stop() {
			clearInterval(poll);
			write(REPORT_OFF);
		},
	};
}
