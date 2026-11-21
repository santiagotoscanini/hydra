// Package stack finds the chain of worktrees a worktree is stacked on.
//
// For main <- b1 <- b2 <- b3, each branch checked out in its own worktree,
// Chain(b3) returns [b1, b2, b3]. Each layer is diffed against its parent
// only, so a service's changes are attributed to the branch (and worktree)
// that owns them, and a running stack can watch every worktree in the chain.
package stack

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Worktree is one entry of `git worktree list`.
type Worktree struct {
	Root   string
	Branch string // short name; empty when detached
	Head   string
}

// Layer is one branch of a stack.
type Layer struct {
	Name   string // branch name, or the directory name when detached
	Root   string // worktree root
	Parent string // what the layer is diffed against: a branch name or the base ref
	Base   string // merge-base commit between Parent and this worktree's HEAD
}

// Options tune parent detection.
type Options struct {
	BaseRef string
	// ParentOverride returns an explicit parent branch for a worktree root
	// (from its .hydra.local.yaml), or "".
	ParentOverride func(root string) string
}

// List returns the repository's worktrees.
func List(dir string) ([]Worktree, error) {
	out, err := git(dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var wts []Worktree
	var cur *Worktree
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			wts = append(wts, Worktree{Root: canon(strings.TrimPrefix(line, "worktree "))})
			cur = &wts[len(wts)-1]
		case cur != nil && strings.HasPrefix(line, "HEAD "):
			cur.Head = strings.TrimPrefix(line, "HEAD ")
		case cur != nil && strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		}
	}
	return wts, nil
}

// Chain returns the layers from the one closest to the base ref up to the
// worktree at root (last). A worktree that isn't stacked on another open
// worktree yields a single layer. Warnings report parents that couldn't be
// told apart, where the worktree was treated as sitting on the base ref.
func Chain(root string, opt Options) ([]Layer, []string, error) {
	root = canon(root)
	wts, err := List(root)
	if err != nil {
		return nil, nil, err
	}
	if len(wts) == 0 {
		return nil, nil, fmt.Errorf("no worktrees found")
	}
	main := wts[0].Root // git lists the main checkout first
	byRoot := map[string]Worktree{}
	byBranch := map[string]Worktree{}
	for _, w := range wts {
		byRoot[w.Root] = w
		if w.Branch != "" {
			byBranch[w.Branch] = w
		}
	}
	self, ok := byRoot[root]
	if !ok {
		return nil, nil, fmt.Errorf("%s is not a worktree of this repository", root)
	}

	var chain []Layer
	var warnings []string
	seen := map[string]bool{}
	for w := self; ; {
		if seen[w.Root] { // can't happen with reflog parents, but never loop
			return chain, append(warnings, "worktree parents form a cycle at "+w.Root+"; stopping there"), nil
		}
		seen[w.Root] = true

		parent, warn, err := findParent(w, wts, main, opt)
		if err != nil {
			return nil, nil, err
		}
		if warn != "" {
			warnings = append(warnings, warn)
		}
		parentRef := opt.BaseRef
		if parent != nil {
			parentRef = parent.Branch
		}
		base, err := git(w.Root, "merge-base", parentRef, "HEAD")
		if err != nil {
			return nil, nil, fmt.Errorf("%s: merge-base %s HEAD: %w", w.Root, parentRef, err)
		}
		name := w.Branch
		if name == "" {
			name = filepath.Base(w.Root)
		}
		chain = append([]Layer{{Name: name, Root: w.Root, Parent: parentRef, Base: strings.TrimSpace(base)}}, chain...)
		if parent == nil {
			return chain, warnings, nil
		}
		w = byBranch[parent.Branch]
	}
}

// findParent returns the open worktree w is stacked on, or nil when w sits
// directly on the base ref.
//
//   - An explicit override wins.
//   - Then the branch w was created from, per w's reflog ("branch: Created
//     from b1"), if it is checked out: that holds even after b1 moves on and
//     the two diverge.
//   - Otherwise the deepest checked-out branch whose tip w contains.
//
// Branches that only share history with w (siblings cut from a commit no
// worktree has checked out) aren't parents: from ancestry alone a sibling
// looks exactly like a parent that moved on. Two contained branches at the
// same commit can't be told apart either; w is then treated as sitting on
// the base ref, with a warning.
func findParent(w Worktree, wts []Worktree, main string, opt Options) (*Worktree, string, error) {
	if opt.ParentOverride != nil {
		if p := opt.ParentOverride(w.Root); p != "" {
			if p == opt.BaseRef {
				return nil, "", nil
			}
			for i := range wts {
				if wts[i].Branch == p {
					return &wts[i], "", nil
				}
			}
			return nil, "", fmt.Errorf("%s: parent %q is not checked out in any worktree", w.Root, p)
		}
	}
	if w.Branch == "" {
		return nil, "", nil
	}
	candidates := func(yield func(*Worktree)) {
		for i := range wts {
			c := &wts[i]
			if c.Root != w.Root && c.Root != main && c.Branch != "" && c.Branch != opt.BaseRef {
				yield(c)
			}
		}
	}

	if from := createdFrom(w); from != "" {
		var hit *Worktree
		candidates(func(c *Worktree) {
			if c.Branch == from {
				hit = c
			}
		})
		// Only if it still shares commits with w past the base (it may have
		// been merged, or w rebased away from it).
		if hit != nil && sharesPastBase(w.Root, hit.Branch, opt.BaseRef) {
			return hit, "", nil
		}
	}

	mbBase, err := git(w.Root, "merge-base", opt.BaseRef, "HEAD")
	if err != nil {
		return nil, "", err
	}
	var best []*Worktree
	bestDepth := 0
	candidates(func(c *Worktree) {
		if !isAncestor(w.Root, c.Branch, "HEAD") || sameCommit(w.Root, c.Branch, "HEAD") {
			return // not contained in w, or w has no commits of its own past it
		}
		n, err := git(w.Root, "rev-list", "--count", strings.TrimSpace(mbBase)+".."+c.Branch)
		if err != nil {
			return
		}
		depth, _ := strconv.Atoi(strings.TrimSpace(n))
		switch {
		case depth == 0:
		case depth > bestDepth:
			best, bestDepth = []*Worktree{c}, depth
		case depth == bestDepth:
			best = append(best, c)
		}
	})
	switch len(best) {
	case 0:
		return nil, "", nil
	case 1:
		return best[0], "", nil
	}
	var names []string
	for _, c := range best {
		names = append(names, c.Branch)
	}
	return nil, fmt.Sprintf("%s could be stacked on %s (same commit); treating it as based on %s (set `parent:` in its .hydra.local.yaml)",
		w.Branch, strings.Join(names, " or "), opt.BaseRef), nil
}

// sharesPastBase reports whether branch and HEAD have commits in common that
// the base doesn't have.
func sharesPastBase(dir, branch, base string) bool {
	mb, err := git(dir, "merge-base", branch, "HEAD")
	if err != nil {
		return false
	}
	return !isAncestor(dir, strings.TrimSpace(mb), base)
}

// createdFrom reads "branch: Created from X" from the branch's reflog.
func createdFrom(w Worktree) string {
	out, err := git(w.Root, "reflog", "show", "--format=%gs", "refs/heads/"+w.Branch)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := lines[len(lines)-1] // oldest entry
	from, ok := strings.CutPrefix(last, "branch: Created from ")
	if !ok || from == "HEAD" {
		return ""
	}
	return strings.TrimPrefix(from, "refs/heads/")
}

func isAncestor(dir, a, b string) bool {
	return exec.Command("git", "-C", dir, "merge-base", "--is-ancestor", a, b).Run() == nil
}

func sameCommit(dir, a, b string) bool {
	x, err1 := git(dir, "rev-parse", a)
	y, err2 := git(dir, "rev-parse", b)
	return err1 == nil && err2 == nil && x == y
}

func canon(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0") // polling must not fight the agent's git
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}
