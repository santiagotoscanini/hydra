// Package changes lists the files a worktree changed relative to a base.
package changes

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// FromBase returns paths that differ between commit base and the worktree's
// current state, untracked files included.
func FromBase(dir, base string) ([]string, error) {
	diff, err := git(dir, "diff", "--name-only", "--no-renames", base)
	if err != nil {
		return nil, err
	}
	untracked, err := git(dir, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range strings.Split(diff+"\n"+untracked, "\n") {
		if f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out, nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	// Polling runs every few seconds next to an agent's own git commands;
	// skip the optional index refresh lock so the two never collide.
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}
