// Package gitx answers the few git questions Hydra needs.
package gitx

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Checkout describes the git checkout a directory belongs to.
type Checkout struct {
	Root      string // top level of this checkout
	Main      string // top level of the main checkout (the baseline)
	CommonDir string // the shared .git directory
}

// IsMain reports whether this is the main checkout rather than a worktree.
func (c Checkout) IsMain() bool { return c.Root == c.Main }

// Find resolves the checkout containing dir.
func Find(dir string) (Checkout, error) {
	root, err := run(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return Checkout{}, err
	}
	common, err := run(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return Checkout{}, err
	}
	root, _ = filepath.EvalSymlinks(root)
	common, _ = filepath.EvalSymlinks(common)
	return Checkout{Root: root, Main: filepath.Dir(common), CommonDir: common}, nil
}

// DefaultBranch is the branch worktrees are compared with when the stack
// file names none:
//
//   - the branch origin calls its default (origin/HEAD, set by `git clone`),
//   - else a local main or master,
//   - else whatever the main checkout has checked out.
func DefaultBranch(co Checkout) string {
	if ref, err := run(co.Main, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil && ref != "" {
		if name := strings.TrimPrefix(ref, "origin/"); hasRef(co.Main, "refs/heads/"+name) {
			return name
		}
		return ref // never checked out locally: compare with the remote's copy
	}
	for _, name := range []string{"main", "master"} {
		if hasRef(co.Main, "refs/heads/"+name) {
			return name
		}
	}
	if name, err := run(co.Main, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil && name != "" {
		return name
	}
	return "main"
}

func hasRef(dir, ref string) bool {
	_, err := run(dir, "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

func run(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
