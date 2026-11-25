package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/journal"
)

// What hydra writes to its journal (see package journal), and where from:
//
//	command  every hydra command, when it ends: its arguments, where it ran,
//	         how long it took, how it exited and the error it printed
//	start    a long-running one (watch, router, dashboard) starting
//	problem  each ✗ and ! a check showed someone, with its fix
//	slot     a worktree taking a slot, or taking a stopped one's
//	process  a process of a stack changing state (from the watcher in a
//	         worktree, the router in main), with its last lines of output
//	         when it failed or restarted
//	usage    every minute, what each stack's processes hold (memory, CPU),
//	         and in main the machine's free memory and swap
//	log      every line the watcher and the router print

// openJournal starts this process's journal for the checkout that holds
// dir. Outside a repository there is none.
func openJournal(dir string) {
	co, err := gitx.Find(dir)
	if err != nil {
		return
	}
	openJournalFor(co)
}

func openJournalFor(co gitx.Checkout) {
	journal.Open(co.CommonDir, checkoutName(co), slotOf(co), shortVersion())
}

// checkoutName is how the journal names a checkout: "main", or the
// worktree's folder.
func checkoutName(co gitx.Checkout) string {
	if co.IsMain() {
		return "main"
	}
	return filepath.Base(co.Root)
}

// shortVersion is the commit hydra was built from, "+" when it had
// uncommitted changes.
func shortVersion() string {
	v := buildVersion()
	rev, _, _ := strings.Cut(v, " ")
	if strings.Contains(v, "uncommitted") {
		rev += "+"
	}
	return rev
}

// quiet commands leave no trace: reading the journal isn't news.
var quiet = map[string]bool{"help": true, "-h": true, "--help": true, "version": true, "--version": true, "log": true, "report": true}

// isQuiet says whether a command leaves no trace: a quiet one, or the
// readiness check process-compose runs on main's PgBouncer every 2 seconds.
func isQuiet(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "pgbouncer" {
		for _, a := range args {
			if a == "--ready" {
				return true
			}
		}
	}
	return quiet[args[0]]
}

// logCommand records a command once it has ended.
func logCommand(args []string, began time.Time, err error) {
	if isQuiet(args) {
		return
	}
	cwd, _ := os.Getwd()
	f := journal.Fields{"args": args, "cwd": cwd, "ms": time.Since(began).Milliseconds(), "exit": exitStatus(err)}
	var said errProblems
	if err != nil && !errors.As(err, &said) {
		var code errExit
		if !errors.As(err, &code) {
			f["error"] = err.Error()
		}
	}
	journal.Log("command", f)
}

func exitStatus(err error) int {
	var code errExit
	switch {
	case err == nil:
		return 0
	case errors.As(err, &code):
		return int(code)
	}
	return 1
}

// logProblems records what a check showed.
func logProblems(ps []problem) {
	for _, p := range ps {
		level := "warning"
		switch {
		case p.Blocks:
			level = "error"
		case p.Passing:
			level = "waiting"
		}
		journal.Log("problem", journal.Fields{"level": level, "text": p.Text, "fix": p.Fix})
	}
}
