package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
)

// versionCmd says which hydra this is and which process-compose it drives.
// hydra is built from a checkout (Homebrew builds main's head, `make link`
// uses a working copy), so its version is the commit it was built from.
func versionCmd() error {
	exe, _ := os.Executable()
	fmt.Printf("hydra %s\n  %s\n", buildVersion(), exe)
	pcBin, err := exec.LookPath("process-compose")
	if err != nil {
		fmt.Println("process-compose: not installed")
		return nil
	}
	out, _ := exec.Command(pcBin, "version", "--short").Output()
	fmt.Printf("process-compose %s\n  %s\n", strings.TrimSpace(string(out)), pcBin)
	return nil
}

// buildVersion reads what the Go toolchain recorded about the checkout the
// binary was built from.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(unknown build)"
	}
	var rev, at string
	dirty := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.time":
			at = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "(built without version control information)"
	}
	if len(rev) > 9 {
		rev = rev[:9]
	}
	if day, _, ok := strings.Cut(at, "T"); ok {
		at = day
	}
	v := rev + " (" + at
	if dirty {
		v += ", plus uncommitted changes"
	}
	return v + ")"
}
