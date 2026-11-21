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
