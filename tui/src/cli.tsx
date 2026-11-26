// The entry point: `hydra` (no arguments, in a terminal) writes this file
// to its cache and runs it with node, with HYDRA_EXE set to itself.

import React from "react";
import { render } from "ink";
import App from "./App.js";
import { ESC, SELECTION_BG, detectTheme, realBackend } from "./hydra.js";

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
let left = false;
const leave = () => {
	if (left) return;
	left = true;
	process.stdout.write(ESC.altOff + ESC.cursorOn);
};
process.on("exit", leave);

// The selection's background has to suit the terminal's.
const theme = await detectTheme();

const instance = render(<App backend={backend} selectionBg={SELECTION_BG[theme]} home={process.env["HOME"]} />, {
	exitOnCtrlC: false,
	patchConsole: false,
});
await instance.waitUntilExit();
leave();
process.exit(0);
