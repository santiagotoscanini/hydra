// The entry point: `hydra` (no arguments, in a terminal) writes this file
// to its cache and runs it with node, with HYDRA_EXE set to itself.

import React from "react";
import { render } from "ink";
import App from "./App.js";
import { ESC, realBackend } from "./hydra.js";
import { watchTheme, type Theme } from "./theme.js";

// By here every module has loaded: a test runs the bundle this far.
if (!process.stdin.isTTY || !process.stdout.isTTY) {
	console.error("hydra's dashboard needs a terminal; hydra status --all says the same in text");
	process.exit(2);
}

const exe = process.env["HYDRA_EXE"] || "hydra";
const backend = realBackend(exe, process.cwd());

// The alternate screen, as vim and less use: leaving it puts back what the
// terminal showed before.
process.stdout.write(ESC.altOn + ESC.cursorOff + ESC.clear);
// The selection's background and yellow have to suit the terminal's
// background, which can change while the dashboard runs (theme.ts).
const app = (theme: Theme) => <App backend={backend} theme={theme} home={process.env["HOME"]} />;
let instance: ReturnType<typeof render> | undefined;
const theme = watchTheme(process.stdin, (s) => process.stdout.write(s), (t) => instance?.rerender(app(t)));

let left = false;
const leave = () => {
	if (left) return;
	left = true;
	theme.stop();
	process.stdout.write(ESC.altOff + ESC.cursorOn);
};
process.on("exit", leave);

// A moment for the terminal to say which theme it has, so the first frame
// is drawn in it.
await theme.ready;

instance = render(app(theme.theme()), {
	stdin: theme.stdin,
	exitOnCtrlC: false,
	patchConsole: false,
});
await instance.waitUntilExit();
leave();
process.exit(0);
