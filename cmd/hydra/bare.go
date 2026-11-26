package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/stack"
)

// A PR stack's branches that no worktree has checked out (internal/stack,
// branches.go): main <- schema <- core <- web with only web checked out.
// hydra runs web on schema's and core's code, so they are listed, measured
// against their parents and updated like worktrees, parents first. A clean
// merge needs no checkout: git merge-tree merges in memory, git commit-tree
// makes the merge commit (git's message, the author and committer from git
// config) and git update-ref moves the branch, only if it is still where it
// was. Nothing restarts: nothing runs it. A conflict needs a worktree; the
// update says how to make a temporary one.

// stackView is the repository's stacks, read once for an update or a
// listing.
type stackView struct {
	base  string
	st    *stack.Stacks
	roots map[string]string // branch -> the worktree it is checked out in
}

func loadStacks(mainRoot, base string, wts []stack.Worktree) (*stackView, error) {
	st, err := stack.FindStacks(mainRoot, base, wts)
	if err != nil {
		return nil, fmt.Errorf("finding the stacks' branches: %w", err)
	}
	v := &stackView{base: base, st: st, roots: map[string]string{}}
	for _, w := range wts {
		if w.Branch != "" {
			v.roots[w.Branch] = w.Root
		}
	}
	return v, nil
}

func stackOptions(base string) stack.Options {
	return stack.Options{
		BaseRef: base,
		ParentOverride: func(root string) string {
			lf, _ := readLocal(filepath.Join(root, ".hydra.local.yaml"))
			if lf == nil {
				return ""
			}
			return lf.Parent
		},
	}
}

// place is where the worktree at root sits: the branch an update merges
// into it, and its chain, bottom first and its own last. v may be nil:
// the worktrees only.
func (v *stackView) place(root, base string) (parent string, chain []string, err error) {
	var st *stack.Stacks
	if v != nil {
		st = v.st
	}
	return stack.Place(root, stackOptions(base), st)
}

// bareChain is the chain of a branch with no checkout, bottom first.
func (v *stackView) bareChain(branch string) []string {
	return v.st.BareChain(branch, func(b string) []string {
		root, ok := v.roots[b]
		if !ok {
			return nil
		}
		_, chain, err := v.place(root, v.base)
		if err != nil {
			return nil
		}
		return chain
	})
}

// bareParent is the branch a branch with no checkout sits on.
func (v *stackView) bareParent(branch string) string {
	if p, ok := v.st.Parent(branch); ok {
		return p
	}
	return v.base
}

// decideBare is decide for a branch with no checkout: the same answers,
// worked out with git in the main checkout.
func (u *wtUpdate) decideBare(base, mainCommit string, dry bool) {
	ours, err := gitOut(u.main, "rev-parse", "--verify", "--quiet", "refs/heads/"+u.name+"^{commit}")
	if err != nil || ours == "" {
		u.note, u.hand = "left alone: the branch can't be read", true
		return
	}
	theirs := mainCommit
	if u.parent != base {
		theirs, err = gitOut(u.main, "rev-parse", "--verify", "--quiet", "refs/heads/"+u.parent+"^{commit}")
		if err != nil || theirs == "" {
			u.note, u.hand = "left alone: its parent branch "+u.parent+" can't be read", true
			return
		}
	}
	u.ours, u.theirs = ours, theirs
	if gitx.IsAncestor(u.main, theirs, ours) {
		u.note = "up to date"
		if u.parent != base {
			u.note = "up to date with " + shortBranch(u.parent) + ", which it is stacked on"
		}
		return
	}
	behind, _ := gitOut(u.main, "rev-list", "--count", ours+".."+theirs)
	u.behind, _ = strconv.Atoi(behind)
	would := map[bool]string{true: "would be ", false: ""}[dry]
	if gitx.IsAncestor(u.main, ours, theirs) {
		u.action, u.note = "fast-forward", fmt.Sprintf("%sfast-forwarded to %s (%s; it has none of its own), without a checkout", would, u.parent, commits(behind))
		return
	}
	tree, files, err := mergeTreeCached(u.main, ours, theirs)
	switch {
	case err != nil:
		u.note, u.hand = fmt.Sprintf("left alone: %v", err), true
	case len(files) == 0:
		if t, _ := gitOut(u.main, "rev-parse", theirs+"^{tree}"); tree == t {
			u.note = fmt.Sprintf("left alone: everything on it is already in %s (merged upstream?); it can be deleted", u.parent)
			return
		}
		u.tree = tree
		u.action, u.note = "merge", fmt.Sprintf("%s %smerged in (%s), without a checkout", u.parent, would, commits(behind))
	default:
		u.note, u.hand = fmt.Sprintf("CONFLICTS with %s in %d file%s (%s behind); it has no checkout", u.parent, len(files), plural(len(files)), commits(behind)), true
		u.conflictDetail(files)
		tmp := filepath.Join(os.TempDir(), "hydra-merge-"+branchSlug(u.name))
		u.detail = append(u.detail,
			"resolve:  in a temporary worktree: git -C "+shQuote(u.main)+" worktree add "+shQuote(tmp)+" "+shQuote(u.name),
			"          then: cd "+shQuote(tmp)+" && git merge "+shQuote(u.parent)+"; resolve, commit",
			"          then: git -C "+shQuote(u.main)+" worktree remove "+shQuote(tmp))
	}
}

// applyBare does the merge decideBare chose, without a checkout.
func (u *wtUpdate) applyBare() {
	// Checked out since: moving the branch would leave that worktree's
	// files behind its HEAD.
	if wts, err := stack.List(u.main); err == nil {
		for _, w := range wts {
			if w.Branch == u.name {
				u.note, u.hand = fmt.Sprintf("NOT merged: it was checked out in %s meanwhile; run the update again", w.Root), true
				return
			}
		}
	}
	next := u.theirs
	if u.action == "merge" {
		// Merged again, not from the cache: what is committed is what git
		// merges now, in this repository's objects.
		tree, files, err := mergeTree(u.main, u.ours, u.theirs)
		if err != nil || len(files) > 0 {
			u.note, u.hand = fmt.Sprintf("NOT merged: merging %s again didn't go cleanly (%v %s); the branch is as it was", u.parent, err, someFiles(files)), true
			return
		}
		u.tree = tree
		msg := fmt.Sprintf("Merge branch '%s' into %s", u.parent, u.name)
		c, err := gitRun(u.main, "commit-tree", u.tree, "-p", u.ours, "-p", u.theirs, "-m", msg)
		if err != nil {
			u.note, u.hand = fmt.Sprintf("NOT merged: %v; the branch is as it was", err), true
			return
		}
		next = c
	}
	// The old value guards it: if the branch moved since decide looked, git
	// refuses and nothing changes.
	reason := fmt.Sprintf("hydra update: %s %s (no checkout)", map[bool]string{true: "merge", false: "fast-forward to"}[u.action == "merge"], u.parent)
	if _, err := gitRun(u.main, "update-ref", "-m", reason, "refs/heads/"+u.name, next, u.ours); err != nil {
		u.note, u.hand = fmt.Sprintf("NOT merged: the branch moved while hydra merged (%v); it is as the other change left it, run the update again", err), true
		return
	}
	u.note += " → " + gitx.Short(next)
}

// gitRun runs git for a change (no GIT_OPTIONAL_LOCKS) and returns what it
// printed.
func gitRun(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		if msg := lastLine(stderrOf(err)); msg != "" {
			return "", fmt.Errorf("git %s: %s", args[0], msg)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

// conflictDetail lists the files a merge conflicts in, a few of them.
func (u *wtUpdate) conflictDetail(files []string) {
	const show = 4
	for i, f := range files {
		if i == show && len(files) > show+1 {
			u.detail = append(u.detail, fmt.Sprintf("conflict: … and %d more", len(files)-show))
			break
		}
		u.detail = append(u.detail, "conflict: "+f)
	}
}

// branchSlug is a branch's last part, for a directory name.
func branchSlug(b string) string {
	if i := strings.LastIndex(b, "/"); i >= 0 {
		b = b[i+1:]
	}
	return b
}

// bareRow is a branch with no checkout as `hydra status` shows it: what an
// update would do to it.
type bareRow struct {
	name string
	u    *wtUpdate
}

// bareRows works out what an update would do to each branch with no
// checkout, or to those of them named in only.
func bareRows(view *stackView, mainRoot, base, mainHead string, only []string) []bareRow {
	var rows []bareRow
	for _, b := range view.st.Bare {
		if only != nil && !contains(only, b.Name) {
			continue
		}
		u := &wtUpdate{name: b.Name, main: mainRoot}
		u.inspect(base, view)
		u.decide(mainRoot, base, mainHead, true)
		rows = append(rows, bareRow{b.Name, u})
	}
	return rows
}

// problem is how `hydra status` says a branch with no checkout is behind
// its parent: a warning, since nothing runs it, but every worktree stacked
// on it runs without what its parent has.
func (r bareRow) problem(above []string) *problem {
	u := r.u
	if u.behind == 0 && !u.hand {
		return nil
	}
	on := ""
	if len(above) > 0 {
		on = " " + strings.Join(above, ", ") + ", stacked on it, run" + map[bool]string{true: "s", false: ""}[len(above) == 1] + " without them."
	}
	text := fmt.Sprintf("%s has no checkout and is %s behind %s:%s", shortBranch(r.name), commits(strconv.Itoa(u.behind)), shortBranch(u.parent), on)
	if strings.HasPrefix(u.note, "CONFLICTS") {
		var files []string
		for _, d := range u.detail {
			if f, ok := strings.CutPrefix(d, "conflict: "); ok {
				files = append(files, f)
			}
		}
		tmp := filepath.Join(os.TempDir(), "hydra-merge-"+branchSlug(r.name))
		return &problem{
			Text: fmt.Sprintf("%s Merging %s into it conflicts in %s.", text, shortBranch(u.parent), someFiles(files)),
			Fix:  fmt.Sprintf("resolve it in a temporary worktree: git worktree add %s %s, merge %s there, commit, git worktree remove %s", shQuote(tmp), shQuote(r.name), u.parent, shQuote(tmp)),
		}
	}
	if u.hand {
		return &problem{Text: text + " " + u.note}
	}
	return &problem{Text: text, Fix: "hydra update --branch " + shQuote(r.name) + "   (merges it without a checkout; `hydra update` does every branch, parents first)"}
}

// chains is every worktree's chain, by branch: what stackedOn reads.
func (v *stackView) chains() map[string][]string {
	out := map[string][]string{}
	for b, root := range v.roots {
		if b == v.base {
			continue
		}
		if _, chain, err := v.place(root, v.base); err == nil {
			out[b] = chain
		}
	}
	return out
}

// stackedOn names the worktrees whose chain has branch under their own.
func stackedOn(chains map[string][]string, branch string) []string {
	var out []string
	for b, chain := range chains {
		if len(chain) > 1 && contains(chain[:len(chain)-1], branch) {
			out = append(out, shortBranch(b))
		}
	}
	sort.Strings(out)
	return out
}
