package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/pc"
	"github.com/santiagotoscanini/hydra/internal/stack"
)

// Bringing main and every worktree up to date, with an agent resolving the
// conflicts `hydra update` leaves alone. The worktrees tool says, for each
// worktree, what merging its parent would do, in the order to do it: a
// branch stacked on another is merged after it, so it takes in what its
// parent just took from main. update_main and update_worktree run
// `hydra update --main-only` and `hydra update --here` (the latter in
// another worktree), so they behave exactly as the commands do. The
// update_all prompt is the workflow that ties them together.

// wtEntry is one worktree, as the worktrees tool lists it, or a branch of
// a stack that no worktree has checked out (Checkout false: no path, its
// branch as its name, nothing runs it).
type wtEntry struct {
	Order    int    `json:"order"`  // 1 is main; the worktrees follow, parents first
	Name     string `json:"name"`   // the folder's name; the branch, with no checkout
	Path     string `json:"path"`   // "" with no checkout
	Branch   string `json:"branch"` // "" when detached
	Checkout bool   `json:"checkout"`
	// Parent is what an update merges in: main's branch, or the branch it
	// is stacked on.
	Parent string `json:"parent"`
	Depth  int    `json:"depth"` // 1: on main; 2: on a worktree that is on main; ...
	Behind int    `json:"behind"`
	Ahead  int    `json:"ahead"`
	// Conflicts: the files merging its parent in conflicts in, every one.
	Conflicts []string `json:"conflicts"`
	// Uncommitted: tracked files with uncommitted changes. An update leaves
	// such a worktree alone.
	Uncommitted int `json:"uncommitted"`
	// Next is what to do: "merge" (update_worktree merges it cleanly),
	// "resolve" (merging conflicts: resolve them by hand), "skip" (left
	// alone: uncommitted work, a merge under way, ...; note says why) or
	// "nothing" (up to date).
	Next  string `json:"next"`
	Note  string `json:"note"`  // hydra update's line for it
	Stack string `json:"stack"` // "up", "down", "idle-stopped", or "none" with no checkout
	Slot  int    `json:"slot"`  // 0: it never ran
	// PR is its pull request (prs.go); absent when none is known. One merged
	// or closed has nothing left to do: next is "skip".
	PR *snapPR `json:"pr,omitempty"`
}

type mainEntry struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Commit string `json:"commit"`
	// BehindOrigin: commits its upstream has that it doesn't, as of the last
	// fetch (update_main with dry_run fetches and says).
	BehindOrigin int  `json:"behindOrigin"`
	Uncommitted  int  `json:"uncommitted"`
	StackUp      bool `json:"stackUp"`
}

type worktreesResult struct {
	Main      mainEntry `json:"main"`
	Worktrees []wtEntry `json:"worktrees"` // in the order to update them
}

func mcpWorktrees(_ context.Context, c *mcpCall) (toolResult, error) {
	os.Setenv("GIT_OPTIONAL_LOCKS", "0")
	if err := c.decode(&struct{}{}); err != nil {
		return toolResult{}, err
	}
	res, err := listWorktrees(c.co)
	if err != nil {
		return toolResult{}, err
	}
	return toolResult{summary: worktreesSummary(res), data: res}, nil
}

// listWorktrees is main and every worktree, in the order to update them.
func listWorktrees(co gitx.Checkout) (*worktreesResult, error) {
	ms, err := loadMain(co)
	if err != nil {
		return nil, err
	}
	mainRoot, base := ms.target.checkout.Root, ms.target.stack.Hydra.BaseRef
	res := &worktreesResult{Main: mainEntry{
		Path: mainRoot, Branch: ms.Branch, Commit: gitx.Short(ms.Head),
		BehindOrigin: ms.Behind, Uncommitted: len(ms.Dirty), StackUp: ms.Up,
	}}
	wts, err := stack.List(mainRoot)
	if err != nil {
		return nil, err
	}
	slotOf := map[string]int{}
	for _, e := range mustList(co.CommonDir) {
		slotOf[e.Root] = e.Slot
	}
	view, err := loadStacks(mainRoot, base, wts)
	if err != nil {
		return nil, err
	}
	var entries []*wtEntry
	for _, wt := range wts {
		if wt.Root != mainRoot {
			entries = append(entries, &wtEntry{Name: filepath.Base(wt.Root), Path: wt.Root, Branch: wt.Branch, Checkout: true, Slot: slotOf[wt.Root], Conflicts: []string{}})
		}
	}
	for _, b := range view.st.Bare {
		entries = append(entries, &wtEntry{Name: b.Name, Branch: b.Name, Conflicts: []string{}})
	}
	// Each takes a few git commands, and a merge-tree when it is behind.
	var wg sync.WaitGroup
	sem := make(chan struct{}, slowAtOnce)
	for _, e := range entries {
		wg.Add(1)
		go func(e *wtEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			e.inspect(mainRoot, base, ms.Head, view)
		}(e)
	}
	wg.Wait()
	prs := shownPRs(ms, wts, view)
	for _, e := range entries {
		e.PR = prs.of(e.Branch)
		if e.PR.Done() {
			e.Next = "skip"
			e.Note = "left alone: its PR is " + e.PR.Long() + ", nothing left to do here"
		}
	}
	res.Worktrees = updateOrder(entries, base)
	return res, nil
}

// inspect works out where the worktree stands against its parent, the way
// hydra update does, and every file merging the parent in conflicts in.
func (e *wtEntry) inspect(mainRoot, base, mainHead string, view *stackView) {
	u := &wtUpdate{root: e.Path, name: e.Branch, main: mainRoot}
	u.inspect(base, view)
	u.decide(mainRoot, base, mainHead, true)
	e.Parent, e.Depth, e.Note = u.parent, u.depth, u.note
	if !e.Checkout {
		e.Stack = "none"
		e.inspectBare(u)
		return
	}
	switch {
	case pc.Unix(socketFor(e.Path)).Alive():
		e.Stack = "up"
	case readIdleNote(e.Path) != nil:
		e.Stack = "idle-stopped"
	default:
		e.Stack = "down"
	}
	if e.Branch == "" {
		e.Next = "nothing"
		return
	}
	if _, err := os.Stat(e.Path); err != nil {
		e.Next = "skip"
		return
	}
	// A commit, not a branch name: mainConflicts caches by the pair.
	theirs := mainHead
	if u.parent != base {
		c, err := gitOut(e.Path, "rev-parse", "--verify", "--quiet", "refs/heads/"+u.parent+"^{commit}")
		if err != nil || c == "" {
			e.Next = "skip"
			e.Note = "left alone: its parent branch " + u.parent + " can't be read"
			return
		}
		theirs = c
	}
	if out, err := gitOut(e.Path, "rev-list", "--left-right", "--count", theirs+"...HEAD"); err == nil {
		if f := strings.Fields(out); len(f) == 2 {
			e.Behind, _ = strconv.Atoi(f[0])
			e.Ahead, _ = strconv.Atoi(f[1])
		}
	}
	if out, _ := gitOut(e.Path, "status", "--porcelain", "--untracked-files=no"); out != "" {
		e.Uncommitted = len(strings.Split(out, "\n"))
	}
	if e.Behind > 0 && !gitx.IsAncestor(e.Path, "HEAD", theirs) {
		if head, err := gitx.Head(e.Path); err == nil {
			if files, err := mainConflicts(e.Path, head, theirs); err == nil {
				e.Conflicts = files
			}
		}
	}
	e.Next = nextOf(u)
}

// inspectBare is the rest of inspect for a branch with no checkout: how far
// it is from its parent, and what merging it conflicts in.
func (e *wtEntry) inspectBare(u *wtUpdate) {
	if u.ours != "" && u.theirs != "" {
		if out, err := gitOut(u.main, "rev-list", "--left-right", "--count", u.theirs+"..."+u.ours); err == nil {
			if f := strings.Fields(out); len(f) == 2 {
				e.Behind, _ = strconv.Atoi(f[0])
				e.Ahead, _ = strconv.Atoi(f[1])
			}
		}
		if e.Behind > 0 && !gitx.IsAncestor(u.main, u.ours, u.theirs) {
			if _, files, err := mergeTreeCached(u.main, u.ours, u.theirs); err == nil {
				e.Conflicts = files
			}
		}
	}
	e.Next = nextOf(u)
}

func nextOf(u *wtUpdate) string {
	switch {
	case u.action != "":
		return "merge"
	case strings.HasPrefix(u.note, "CONFLICTS"):
		return "resolve"
	case u.hand:
		return "skip"
	default:
		return "nothing"
	}
}

// updateOrder puts the worktrees in the order to update them: each one
// right before the branches stacked on it, depth first, so a chain reads
// from its bottom up and a parent is always done before its children.
// Siblings go by name.
func updateOrder(entries []*wtEntry, base string) []wtEntry {
	byBranch := map[string]*wtEntry{}
	for _, e := range entries {
		if e.Branch != "" {
			byBranch[e.Branch] = e
		}
	}
	children := map[string][]*wtEntry{}
	var roots []*wtEntry
	for _, e := range entries {
		if p, ok := byBranch[e.Parent]; ok && e.Parent != base && p != e {
			children[e.Parent] = append(children[e.Parent], e)
		} else {
			roots = append(roots, e)
		}
	}
	byName := func(l []*wtEntry) {
		sort.Slice(l, func(i, j int) bool { return l[i].Name < l[j].Name })
	}
	out := []wtEntry{}
	seen := map[*wtEntry]bool{}
	var walk func(e *wtEntry, depth int)
	walk = func(e *wtEntry, depth int) {
		if seen[e] {
			return
		}
		seen[e] = true
		e.Depth, e.Order = depth, len(out)+2 // main is 1
		out = append(out, *e)
		kids := children[e.Branch]
		byName(kids)
		for _, k := range kids {
			walk(k, depth+1)
		}
	}
	byName(roots)
	for _, r := range roots {
		walk(r, 1)
	}
	for _, e := range entries { // a cycle of parents, which git ancestry can't make; never lost
		walk(e, 1)
	}
	return out
}

func worktreesSummary(r *worktreesResult) string {
	var b strings.Builder
	m := r.Main
	var parts []string
	parts = append(parts, fmt.Sprintf("on %s at %s", orDetached(m.Branch), m.Commit))
	if m.BehindOrigin > 0 {
		parts = append(parts, fmt.Sprintf("behind its upstream by %d (as of the last fetch)", m.BehindOrigin))
	}
	if m.Uncommitted > 0 {
		parts = append(parts, fmt.Sprintf("UNCOMMITTED: %d file%s (update_main refuses)", m.Uncommitted, plural(m.Uncommitted)))
	}
	parts = append(parts, "stack "+upText(m.StackUp, "up", "down"))
	width := len("main")
	for _, w := range r.Worktrees {
		width = max(width, utf8.RuneCountInString(treeIndent(w.Depth)+w.Name))
	}
	fmt.Fprintf(&b, "%2d. %-*s  %s\n", 1, width, "main", strings.Join(parts, " · "))
	for _, w := range r.Worktrees {
		fmt.Fprintf(&b, "%2d. %-*s  %s\n", w.Order, width, treeIndent(w.Depth)+w.Name, strings.Join(w.facts(), " · "))
	}
	if len(r.Worktrees) == 0 {
		b.WriteString("no worktrees: only main to update.\n")
	} else {
		b.WriteString("Update in this order: a worktree after the one it is stacked on (└).\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func treeIndent(depth int) string {
	if depth <= 1 {
		return ""
	}
	return strings.Repeat("  ", depth-2) + "└ "
}

func orDetached(branch string) string {
	if branch == "" {
		return "a detached commit"
	}
	return branch
}

// facts is the worktree's line: its branch when the folder is named
// otherwise, its parent, how far apart they are, what an update does, and
// its stack.
func (w *wtEntry) facts() []string {
	var f []string
	if !w.Checkout {
		f = append(f, "no checkout")
	} else if w.Branch == "" {
		f = append(f, "detached")
	} else if w.Branch != w.Name {
		f = append(f, "branch "+w.Branch)
	}
	f = append(f, "on "+w.Parent)
	if w.PR != nil && !w.PR.Done() {
		f = append(f, "PR "+w.PR.Long())
	}
	switch {
	case w.Behind > 0 && w.Ahead > 0:
		f = append(f, fmt.Sprintf("%d behind, %d ahead", w.Behind, w.Ahead))
	case w.Behind > 0:
		f = append(f, fmt.Sprintf("%d behind", w.Behind))
	case w.Ahead > 0:
		f = append(f, fmt.Sprintf("%d ahead", w.Ahead))
	}
	switch w.Next {
	case "merge":
		f = append(f, "merges cleanly")
	case "resolve":
		f = append(f, fmt.Sprintf("CONFLICTS in %d file%s: %s", len(w.Conflicts), plural(len(w.Conflicts)), someFiles(w.Conflicts)))
	case "skip":
		f = append(f, "SKIP: "+w.Note)
	default:
		f = append(f, "nothing to do")
	}
	if w.Uncommitted > 0 && w.Next != "skip" {
		f = append(f, fmt.Sprintf("uncommitted: %d file%s", w.Uncommitted, plural(w.Uncommitted)))
	}
	if w.Next == "skip" && len(w.Conflicts) > 0 {
		f = append(f, fmt.Sprintf("and merging it conflicts in %d file%s: %s", len(w.Conflicts), plural(len(w.Conflicts)), someFiles(w.Conflicts)))
	}
	if !w.Checkout {
		return f // nothing runs it
	}
	stack := "stack " + w.Stack
	if w.Slot > 0 {
		stack += fmt.Sprintf(" (slot %d)", w.Slot)
	}
	return append(f, stack)
}

func mcpUpdateMain(_ context.Context, c *mcpCall) (toolResult, error) {
	var a struct {
		DryRun bool `json:"dry_run"`
	}
	if err := c.decode(&a); err != nil {
		return toolResult{}, err
	}
	args := []string{"update", "--main-only"}
	if a.DryRun {
		args = append(args, "--dry-run")
	}
	out, err := c.hydraIn(c.co.Main, args...)
	if err != nil {
		return toolResult{}, err
	}
	return toolResult{summary: out, data: map[string]any{"output": out, "dryRun": a.DryRun}}, nil
}

func mcpUpdateWorktree(_ context.Context, c *mcpCall) (toolResult, error) {
	var a struct {
		Worktree string `json:"worktree"`
		DryRun   bool   `json:"dry_run"`
		Since    string `json:"since"`
	}
	if err := c.decode(&a); err != nil {
		return toolResult{}, err
	}
	root, err := findWorktree(c.co, a.Worktree)
	if err != nil {
		if b := findBare(c.co, a.Worktree); b != "" {
			return updateBareTool(c, b, a.DryRun)
		}
		return toolResult{}, err
	}
	if a.Since != "" && !gitx.IsAncestor(root, a.Since, "HEAD") {
		return toolResult{}, fmt.Errorf("since: %q isn't a commit that %s's HEAD descends from; pass the commit it was at before you merged", a.Since, filepath.Base(root))
	}
	args := []string{"update", "--here"}
	if a.DryRun {
		args = append(args, "--dry-run")
	}
	out, runErr := c.hydraIn(root, args...)
	data := map[string]any{"worktree": filepath.Base(root), "path": root, "output": out, "dryRun": a.DryRun}
	if runErr == nil {
		if a.Since != "" && !a.DryRun {
			// A merge made by hand: hydra update had nothing to merge, so
			// it restarted nothing. What the merge touched restarts now.
			head, err := gitx.Head(root)
			if err != nil {
				return toolResult{}, err
			}
			said := restartInWorktree(root, a.Since, head, 15*time.Minute)
			if said == "" {
				said = "nothing to restart since " + gitx.Short(a.Since) + ": its stack isn't up, or nothing it runs changed"
			}
			out += "\n" + said
			data["output"], data["restarted"] = out, said
		}
		return toolResult{summary: out, data: data}, nil
	}
	// A conflict: say every file, and how to go on. The worktree is as it
	// was: the update tried the merge in memory only.
	os.Setenv("GIT_OPTIONAL_LOCKS", "0")
	if r, err := listWorktrees(c.co); err == nil {
		for _, w := range r.Worktrees {
			if w.Path == root && w.Next == "resolve" {
				return toolResult{}, fmt.Errorf("%w\n\nThe worktree was left as it was. Merging %s conflicts in %d file%s:\n  %s\n"+
					"Resolve them with git in that worktree: cd %s && git merge %s; fix each file, run the checks, commit the merge (git's message). "+
					"Then call update_worktree again, with since: the commit it was at before (git rev-parse HEAD before merging): it restarts what the merge touched.",
					runErr, w.Parent, len(w.Conflicts), plural(len(w.Conflicts)), strings.Join(w.Conflicts, "\n  "), shQuote(root), w.Parent)
			}
		}
	}
	return toolResult{}, runErr
}

// findBare names the stack branch with no checkout that name is, or "".
func findBare(co gitx.Checkout, name string) string {
	ms, err := loadMain(co)
	if err != nil {
		return ""
	}
	wts, err := stack.List(co.Main)
	if err != nil {
		return ""
	}
	view, err := loadStacks(co.Main, ms.target.stack.Hydra.BaseRef, wts)
	if err != nil || !view.st.IsBare(name) {
		return ""
	}
	return name
}

// updateBareTool is update_worktree for a branch with no checkout: hydra
// update --branch, which merges without one. A conflict needs one: the
// error says how to make a temporary worktree for it.
func updateBareTool(c *mcpCall, branch string, dry bool) (toolResult, error) {
	args := []string{"update", "--branch", branch}
	if dry {
		args = append(args, "--dry-run")
	}
	out, runErr := c.hydraIn(c.co.Main, args...)
	data := map[string]any{"worktree": branch, "branch": branch, "checkout": false, "output": out, "dryRun": dry}
	if runErr == nil {
		return toolResult{summary: out, data: data}, nil
	}
	os.Setenv("GIT_OPTIONAL_LOCKS", "0")
	if r, err := listWorktrees(c.co); err == nil {
		for _, w := range r.Worktrees {
			if !w.Checkout && w.Branch == branch && w.Next == "resolve" {
				tmp := filepath.Join(os.TempDir(), "hydra-merge-"+branchSlug(branch))
				return toolResult{}, fmt.Errorf("%w\n\nThe branch was left as it was. It has no checkout, and merging %s conflicts in %d file%s:\n  %s\n"+
					"Resolve them in a temporary worktree: git -C %s worktree add %s %s; cd %s && git merge %s; fix each file, run the checks, commit the merge (git's message). "+
					"Then remove it: git -C %s worktree remove %s. Then call worktrees again: the branches stacked on it come next.",
					runErr, w.Parent, len(w.Conflicts), plural(len(w.Conflicts)), strings.Join(w.Conflicts, "\n  "),
					shQuote(c.co.Main), shQuote(tmp), shQuote(branch), shQuote(tmp), shQuote(w.Parent), shQuote(c.co.Main), shQuote(tmp))
			}
		}
	}
	return toolResult{}, runErr
}

// findWorktree finds a worktree of the repository by its path, its
// folder's name or its branch. The main checkout isn't one: update_main
// updates it.
func findWorktree(co gitx.Checkout, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("worktree is required: a name or path from the worktrees tool")
	}
	wts, err := stack.List(co.Main)
	if err != nil {
		return "", err
	}
	want := name
	if filepath.IsAbs(name) {
		if real, err := filepath.EvalSymlinks(name); err == nil {
			want = real
		}
	}
	var hits, names []string
	for _, wt := range wts {
		if wt.Root == want || filepath.Base(wt.Root) == name || wt.Branch == name {
			hits = append(hits, wt.Root)
		}
		if wt.Root != co.Main {
			names = append(names, filepath.Base(wt.Root))
		}
	}
	switch {
	case len(hits) == 1 && hits[0] == co.Main:
		return "", fmt.Errorf("%s is the main checkout; update_main updates it", hits[0])
	case len(hits) == 1:
		return hits[0], nil
	case len(hits) > 1:
		return "", fmt.Errorf("%q names %d worktrees (%s); pass its path", name, len(hits), strings.Join(hits, ", "))
	}
	sort.Strings(names)
	return "", fmt.Errorf("no worktree named %q; the worktrees are: %s", name, strings.Join(names, ", "))
}
