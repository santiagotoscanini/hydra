package stack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// A PR stack's branches don't all have a worktree: main <- schema <- core
// <- web, with only web checked out. hydra runs web on the code of every
// branch under it, so schema and core belong in its chain: shown, and
// updated with their parents, parents first.
//
// Where a stack's branches come from:
//
//   - gh-stack (the `gh stack` extension) keeps its stacks in .git/gh-stack:
//     each stack's trunk and its branches, bottom first. When a worktree's
//     branch is in one of them, that says what it sits on.
//   - Otherwise, ancestry: the local branches a worktree's branch contains
//     that main doesn't, when they form a strict chain (each contains the
//     one under it). A branch merged in from elsewhere, or two unrelated
//     ones, aren't a chain, and nothing is inferred.
//
// Only branches under a worktree's branch are stack branches: a branch
// nobody runs needs nothing. Branches merged into main, and backup/*, never
// are.

// Bare is a stack branch that no worktree has checked out.
type Bare struct {
	Name   string
	Head   string
	Parent string // the branch it sits on, or the base ref
	Source string // "gh-stack" or "ancestry"
}

// Stacks is what the stack branches say about parents, beyond what the
// worktrees say (Chain).
type Stacks struct {
	Base    string
	Heads   map[string]string // every local branch: its commit
	Bare    []Bare            // by name
	parents map[string]string // branch -> the branch it sits on, or Base
}

// Parent is the branch the stacks say branch sits on.
func (s *Stacks) Parent(branch string) (string, bool) {
	if s == nil {
		return "", false
	}
	p, ok := s.parents[branch]
	return p, ok
}

// IsBare says whether branch is a stack branch with no checkout.
func (s *Stacks) IsBare(branch string) bool {
	return s.bare(branch) != nil
}

func (s *Stacks) bare(branch string) *Bare {
	if s == nil {
		return nil
	}
	for i := range s.Bare {
		if s.Bare[i].Name == branch {
			return &s.Bare[i]
		}
	}
	return nil
}

// stacksCache keeps the last answer per repository: it changes only when
// a branch moves, and the dashboard asks every couple of seconds.
var stacksCache = struct {
	sync.Mutex
	m map[string]stacksAt
}{m: map[string]stacksAt{}}

type stacksAt struct {
	key string
	s   *Stacks
}

// FindStacks finds the stack branches of the repository whose main checkout
// is at main. wts are its worktrees (List). It costs one git command while
// no branch moved, three when one did.
func FindStacks(main, base string, wts []Worktree) (*Stacks, error) {
	common, err := commonDir(main)
	if err != nil {
		return nil, err
	}
	refs, err := git(main, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/remotes/*/"+base)
	if err != nil {
		return nil, err
	}
	ghFile := filepath.Join(common, "gh-stack")
	gh := ""
	if fi, err := os.Stat(ghFile); err == nil {
		gh = fmt.Sprintf("%d %d", fi.Size(), fi.ModTime().UnixNano())
	}
	var checked []string
	for _, w := range wts {
		if w.Root != main && w.Branch != "" {
			checked = append(checked, w.Branch)
		}
	}
	sort.Strings(checked)
	key := strings.Join([]string{base, refs, gh, strings.Join(checked, " ")}, "\n")
	stacksCache.Lock()
	hit, ok := stacksCache.m[main]
	stacksCache.Unlock()
	if ok && hit.key == key {
		return hit.s, nil
	}
	s, err := findStacks(main, base, refs, ghFile, checked)
	if err != nil {
		return nil, err
	}
	stacksCache.Lock()
	stacksCache.m[main] = stacksAt{key, s}
	stacksCache.Unlock()
	return s, nil
}

var commonDirs sync.Map // main checkout -> its git directory

func commonDir(main string) (string, error) {
	if d, ok := commonDirs.Load(main); ok {
		return d.(string), nil
	}
	d, err := git(main, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	commonDirs.Store(main, d)
	return d, nil
}

func findStacks(main, base, refs, ghFile string, checked []string) (*Stacks, error) {
	s := &Stacks{Base: base, Heads: map[string]string{}, parents: map[string]string{}}
	var baseRefs []string // the base and its remote-tracking branches
	for _, line := range strings.Split(refs, "\n") {
		oid, ref, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if name, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
			s.Heads[name] = oid
			if name == base {
				baseRefs = append(baseRefs, ref)
			}
		} else {
			baseRefs = append(baseRefs, ref)
		}
	}
	if len(baseRefs) == 0 {
		return s, nil // no base branch to measure against: nothing is a stack
	}
	g, err := loadGraph(main, baseRefs)
	if err != nil {
		return nil, err
	}
	isChecked := map[string]bool{}
	for _, b := range checked {
		isChecked[b] = true
	}
	// A stack branch: a local branch with commits main doesn't have.
	live := func(b string) bool {
		h, ok := s.Heads[b]
		return ok && b != base && !strings.HasPrefix(b, "backup/") && g.has(h)
	}
	var bare []Bare
	addBare := func(name, parent, source string) {
		if !isChecked[name] && s.bare(name) == nil {
			for _, b := range bare {
				if b.Name == name {
					return
				}
			}
			bare = append(bare, Bare{Name: name, Head: s.Heads[name], Parent: parent, Source: source})
		}
	}

	// gh-stack first: what the developer said.
	fromGH := map[string]bool{}
	for _, st := range readGHStacks(ghFile) {
		if st.trunk != base {
			continue
		}
		var members []string
		for _, b := range st.branches {
			if live(b) {
				members = append(members, b)
			}
		}
		top := -1 // the highest branch with a checkout
		for i, b := range members {
			if isChecked[b] {
				top = i
			}
		}
		for i := 0; i <= top; i++ {
			b := members[i]
			if fromGH[b] {
				continue // in an earlier stack too: the first says
			}
			parent := base
			if i > 0 {
				parent = members[i-1]
			}
			fromGH[b] = true
			s.parents[b] = parent
			addBare(b, parent, "gh-stack")
		}
	}

	// Then ancestry, for the checked-out branches gh-stack doesn't know.
	var candidates []string
	for b := range s.Heads {
		if live(b) {
			candidates = append(candidates, b)
		}
	}
	sort.Strings(candidates)
	for _, b := range checked {
		if fromGH[b] || !live(b) {
			continue
		}
		var under []string
		at := map[string]string{} // each one's commit that b has
		for _, c := range candidates {
			if c == b {
				continue
			}
			if x, ok := g.under(s.Heads[b], s.Heads[c]); ok && x != s.Heads[b] {
				under = append(under, c)
				at[c] = x
			}
		}
		chain, ok := g.strictChain(oneEach(b, under, at, isChecked), at)
		if !ok {
			continue
		}
		hasBare := false
		for _, c := range chain {
			hasBare = hasBare || !isChecked[c]
		}
		if !hasBare {
			continue // worktrees only: Chain says it, as before
		}
		s.parents[b] = chain[len(chain)-1]
		for i, c := range chain {
			if isChecked[c] || fromGH[c] {
				continue
			}
			parent := base
			if i > 0 {
				parent = chain[i-1]
			}
			s.parents[c] = parent
			addBare(c, parent, "ancestry")
		}
	}
	sort.Slice(bare, func(i, j int) bool { return bare[i].Name < bare[j].Name })
	s.Bare = bare
	return s, nil
}

// graph is the commits of the local branches that the base doesn't have,
// each with its parents among them.
type graph struct {
	parents map[string][]string
	memo    map[string]map[string]bool
}

// loadGraph reads, in one git command, every commit on a local branch
// that none of baseRefs has.
func loadGraph(dir string, baseRefs []string) (*graph, error) {
	args := append([]string{"rev-list", "--parents", "--branches", "--not"}, baseRefs...)
	out, err := git(dir, args...)
	if err != nil {
		return nil, err
	}
	g := &graph{parents: map[string][]string{}, memo: map[string]map[string]bool{}}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 {
			g.parents[f[0]] = f[1:]
		}
	}
	return g, nil
}

func (g *graph) has(commit string) bool {
	_, ok := g.parents[commit]
	return ok
}

// reaches says whether to is from or one of its ancestors. Both off the
// base: a path between two such commits never leaves the graph, since a
// commit on it that the base had would give the base to as well.
func (g *graph) reaches(from, to string) bool {
	seen, ok := g.memo[from]
	if !ok {
		seen = map[string]bool{}
		todo := []string{from}
		for len(todo) > 0 {
			c := todo[len(todo)-1]
			todo = todo[:len(todo)-1]
			if seen[c] || !g.has(c) {
				continue
			}
			seen[c] = true
			todo = append(todo, g.parents[c]...)
		}
		g.memo[from] = seen
	}
	return seen[to]
}

// under says whether the branch at commit c sits under the commit b, and
// at which of its commits: c itself when b contains it, or, when c only
// took merges since (its parent merged in, with or without a checkout),
// the commit before them. A branch with commits of its own past what b has
// (a sibling, a prototype cut from the same commit) isn't under b.
func (g *graph) under(b, c string) (string, bool) {
	for x := c; g.has(x); x = g.parents[x][0] {
		if g.reaches(b, x) {
			return x, true
		}
		if len(g.parents[x]) < 2 {
			return "", false
		}
	}
	return "", false
}

// oneEach keeps one branch per commit among those under b. Copies of a
// stack's branches at the same commits (port/3-repository beside
// you/feature/3-repository, made to move the stack elsewhere) would
// otherwise make it no chain at all, so the whole stack would vanish. The
// one kept: a checked-out one, else the one whose name shares the most of
// b's path ("you/feature/"), else the first by name.
func oneEach(b string, under []string, at map[string]string, isChecked map[string]bool) []string {
	score := func(c string) int {
		if isChecked[c] {
			return 1 << 20
		}
		n, bs, cs := 0, strings.Split(b, "/"), strings.Split(c, "/")
		for n < len(bs)-1 && n < len(cs)-1 && bs[n] == cs[n] {
			n++
		}
		return n
	}
	kept := map[string]string{} // commit -> branch
	for _, c := range under {   // in order of name
		if k, ok := kept[at[c]]; !ok || score(c) > score(k) {
			kept[at[c]] = c
		}
	}
	var out []string
	for _, c := range under {
		if kept[at[c]] == c {
			out = append(out, c)
		}
	}
	return out
}

// strictChain orders branches so each contains the one before it, and
// says whether they do: two that don't contain one another (or sit at the
// same commit) aren't a stack. heads: the commit each is measured at.
func (g *graph) strictChain(branches []string, heads map[string]string) ([]string, bool) {
	if len(branches) == 0 {
		return nil, false
	}
	below := map[string]int{}
	for _, a := range branches {
		for _, b := range branches {
			if a != b && g.reaches(heads[a], heads[b]) {
				below[a]++
			}
		}
	}
	out := append([]string(nil), branches...)
	sort.Slice(out, func(i, j int) bool { return below[out[i]] < below[out[j]] })
	for i, b := range out {
		if below[b] != i {
			return nil, false
		}
	}
	return out, true
}

type ghStack struct {
	trunk    string
	branches []string
}

// readGHStacks reads gh-stack's file. Unreadable or absent, there are none:
// ancestry still finds the stacks.
func readGHStacks(path string) []ghStack {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f struct {
		Stacks []struct {
			Trunk struct {
				Branch string `json:"branch"`
			} `json:"trunk"`
			Branches []struct {
				Branch string `json:"branch"`
			} `json:"branches"`
		} `json:"stacks"`
	}
	if json.Unmarshal(data, &f) != nil {
		return nil
	}
	var out []ghStack
	for _, st := range f.Stacks {
		g := ghStack{trunk: st.Trunk.Branch}
		for _, b := range st.Branches {
			g.branches = append(g.branches, b.Branch)
		}
		out = append(out, g)
	}
	return out
}

// Place is where the worktree at root sits in its stack, the branches with
// no checkout counted: the branch an update merges into it (parent), and
// the chain of branches from the bottom up to its own (last). An explicit
// parent (ParentOverride) wins; then the stacks; then what Chain finds
// among the worktrees. st may be nil: worktrees only, as Chain.
func Place(root string, opt Options, st *Stacks) (parent string, chain []string, err error) {
	layers, _, err := Chain(root, opt)
	if err != nil {
		return "", nil, err
	}
	if len(layers) == 0 {
		return opt.BaseRef, nil, nil
	}
	self := layers[len(layers)-1]
	byName := map[string]Layer{}
	for _, l := range layers {
		byName[l.Name] = l
	}
	return parentOf(self.Name, byName, opt.BaseRef, st), walk(self.Name, layers, opt.BaseRef, st), nil
}

// walk follows parents down from branch to the base: bottom first, branch
// last.
func walk(branch string, layers []Layer, base string, st *Stacks) []string {
	byName := map[string]Layer{}
	for _, l := range layers {
		byName[l.Name] = l
	}
	chain := []string{branch}
	seen := map[string]bool{branch: true}
	for x := branch; ; {
		p := parentOf(x, byName, base, st)
		if p == base || p == "" || seen[p] {
			break
		}
		seen[p] = true
		chain = append([]string{p}, chain...)
		x = p
	}
	return chain
}

func parentOf(x string, layers map[string]Layer, base string, st *Stacks) string {
	l, isLayer := layers[x]
	if isLayer && l.Explicit {
		return l.Parent
	}
	if p, ok := st.Parent(x); ok {
		return p
	}
	if isLayer {
		return l.Parent
	}
	return base
}

// BareChain is the chain of a branch with no checkout, bottom first and
// its own last. under gives the chain of a worktree's branch, when the
// stacks lead down to one.
func (s *Stacks) BareChain(branch string, under func(branch string) []string) []string {
	chain := []string{branch}
	seen := map[string]bool{branch: true}
	for x := branch; ; {
		p, ok := s.Parent(x)
		if !ok || p == s.Base || seen[p] {
			return chain
		}
		if !s.IsBare(p) && under != nil {
			if below := under(p); len(below) > 0 {
				return append(below, chain...)
			}
		}
		seen[p] = true
		chain = append([]string{p}, chain...)
		x = p
	}
}
