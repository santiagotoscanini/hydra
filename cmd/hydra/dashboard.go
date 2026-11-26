package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/journal"
)

// The dashboard is bare `hydra` in a terminal: every checkout of the
// repository, what each runs, and one key to start, stop, update or open
// one. It is an Ink (React) app, built from tui/ into one file that is
// embedded here (`npm run build` in tui/ writes it; `make build` runs that).
// hydra writes it to its cache once per build and runs it with node. It
// reads `hydra status --json --watch`, and every key runs the hydra command
// an agent would type, in the checkout it is about.

//go:embed dashboard.mjs
var dashboardJS []byte

// dashboardNode is the oldest Node.js the dashboard runs on (Ink 6's).
const dashboardNode = 20

// wantsDashboard says whether bare `hydra` opens the dashboard: a person at
// a terminal, in a repository set up for hydra. Anything else (an agent, a
// script, a pipe) gets what bare hydra always did: process-compose.
func wantsDashboard(args []string) bool {
	if len(args) > 0 || !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return false
	}
	co, err := gitx.Find(".")
	if err != nil {
		return false
	}
	data, err := os.ReadFile(defaultFile(co))
	return err == nil && config.HasHydra(data)
}

func dashboardCmd() error {
	node, err := exec.LookPath("node")
	if err != nil {
		return fmt.Errorf("the dashboard (bare `hydra` in a terminal) needs Node.js %d or newer, and node isn't on PATH.\n"+
			"Install it (brew install node), or use the commands instead: hydra status --all, hydra up (process-compose's own TUI), hydra attach", dashboardNode)
	}
	if v, err := exec.Command(node, "--version").Output(); err == nil {
		major, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(string(v)), "v"), ".")
		if n, err := strconv.Atoi(major); err == nil && n < dashboardNode {
			return fmt.Errorf("the dashboard needs Node.js %d or newer; %s is %s. hydra status --all and hydra up work without it", dashboardNode, node, strings.TrimSpace(string(v)))
		}
	}
	script, err := dashboardScript()
	if err != nil {
		return err
	}
	exe, err := stableExecutable()
	if err != nil {
		return err
	}
	journal.Log("start", journal.Fields{"what": "dashboard"})
	env := append(os.Environ(), "HYDRA_EXE="+exe)
	return syscall.Exec(node, []string{"node", script}, env)
}

// dashboardScript writes the embedded dashboard to the cache, under a name
// that changes with its contents, and returns its path.
func dashboardScript() (string, error) {
	sum := sha256.Sum256(dashboardJS)
	path := filepath.Join(hydraCacheDir(), "dashboard-"+hex.EncodeToString(sum[:6])+".mjs")
	if have, err := os.ReadFile(path); err == nil && sha256.Sum256(have) == sum {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".dashboard-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(dashboardJS); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	// The ones earlier builds wrote, once nothing can be about to start
	// them (another hydra build may be running too).
	old, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "dashboard-*.mjs"))
	for _, f := range old {
		if info, err := os.Stat(f); err == nil && f != path && time.Since(info.ModTime()) > 24*time.Hour {
			os.Remove(f)
		}
	}
	return path, nil
}
