package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/pc"
	"github.com/santiagotoscanini/hydra/internal/stack"
)

// The state of every checkout of the repository, as JSON: what
// `hydra status --json` prints once, and `hydra status --json --watch`
// prints again every second for the dashboard (bare `hydra`).
//
// What it costs to know differs a lot, so each part is worked out again on
// its own clock. A watch asks the process-compose servers what runs every
// second, and git, once, which worktrees there are and where their HEADs
// are. A worktree's plan walks its working tree (git lists untracked
// files), so it is planned again every half minute while its stack runs and
// every two minutes while it doesn't. Where it stands against main (commits
// behind and ahead, and what `hydra update` would do to it, which tries the
// merge with git merge-tree) only changes when a branch moves: it is worked
// out again when the worktree's HEAD, main's or the branch it is stacked on
// moves, and otherwise every two minutes (for uncommitted work, which keeps
// an update away). A commit in the worktree or in main plans it again too.
// Each second it also looks at each worktree's index file and MERGE_HEAD
// (two stats): when either changed (files staged, a merge begun, resolved
// or aborted), where the worktree stands is worked out again, at most every
// few seconds. The slow parts due for the worktrees that moved go first,
// ahead of the ones only due because main moved or time passed: after an
// update of main, all of them are due at once.
// None of the slow parts ever holds up a snapshot: they run in the
// background and show up in the next. With 30 worktrees, 6 of them up, a
// watch takes about a tenth of one core.

// snapshotVersion changes when a field changes meaning or goes away.
// Adding one doesn't change it.
const snapshotVersion = 1

const (
	planAgeRunning = 30 * time.Second
	planAgeStopped = 2 * time.Minute
	gitAge         = 2 * time.Minute
	gitIndexGap    = 3 * time.Second // the least time between two looks for a changed index
	mainAge        = 10 * time.Second
	memoryAge      = 5 * time.Second
	strayAge       = 15 * time.Second // lsof and ps of the machine
	snapshotEvery  = time.Second
	slowAtOnce     = 4 // background plans and git checks running at a time
)

type snapProblem struct {
	Blocks  bool   `json:"blocks"`
	Passing bool   `json:"passing,omitempty"`
	Text    string `json:"text"`
	Fix     string `json:"fix,omitempty"`
	// Run is the fix as a command to run in the checkout the problem is
	// about, when it is one (a hydra command); "" when it is advice.
	Run string `json:"run,omitempty"`
}

type snapService struct {
	Name string `json:"name"`
	// Runs: "here", "main" (this worktree uses main's copy) or "routed"
	// (main's copy, reached through this worktree's port).
	Runs   string `json:"runs"`
	Off    bool   `json:"off,omitempty"`  // listed here, but off until started
	Used   bool   `json:"used,omitempty"` // main's, and something here calls or waits for it
	State  string `json:"state"`          // "ready", "exited with code 1", "not started", ...
	Health string `json:"health"`         // up, done, starting, off, failed, unknown
	Port   int    `json:"port,omitempty"`
	URL    string `json:"url,omitempty"`
	// Address is the name-based address of the service in this checkout
	// (x-hydra hosts): http://<worktree>.localhost:PORT, main's at localhost.
	Address string `json:"address,omitempty"`
	Why     string `json:"why,omitempty"`
	Memory  int64  `json:"memory,omitempty"` // bytes: the process and its children
}

// snapStray is a program that holds a port of the checkout's stack and is
// part of no stack (see strays.go).
type snapStray struct {
	Port    int    `json:"port"`
	Service string `json:"service"`
	Pid     int    `json:"pid"`
	Command string `json:"command"`
	Dir     string `json:"dir,omitempty"`
	DirGone bool   `json:"dirGone,omitempty"` // a worktree that was removed, most likely
}

type snapSkew struct {
	Service string   `json:"service"`
	Why     string   `json:"why"`
	Files   []string `json:"files"`
}

type snapMain struct {
	Root      string        `json:"root"`
	Branch    string        `json:"branch"`
	Commit    string        `json:"commit"`
	Subject   string        `json:"subject"`
	Behind    int           `json:"behind"` // commits its upstream has that it doesn't
	Dirty     int           `json:"dirty"`  // tracked files with uncommitted changes
	Up        bool          `json:"up"`
	Where     string        `json:"where"` // its process-compose server's address
	Processes []snapService `json:"processes"`
	Memory    int64         `json:"memory"`
	Problems  []snapProblem `json:"problems"`
	Strays    []snapStray   `json:"strays,omitempty"`
}

// snapUpdate is what `hydra update` would do to a worktree now, worked out
// the way the update works it out.
type snapUpdate struct {
	Parent    string   `json:"parent"`           // what is merged in: main, or the branch it is stacked on
	Action    string   `json:"action,omitempty"` // "merge", "fast-forward", "" when it is left alone
	Note      string   `json:"note"`             // the update's line for it
	Hand      bool     `json:"hand,omitempty"`   // someone has to step in: conflicts, uncommitted work
	Conflicts []string `json:"conflicts,omitempty"`
}

type snapWorktree struct {
	Root    string `json:"root"`
	Dir     string `json:"dir"`
	Branch  string `json:"branch"`
	Slot    int    `json:"slot"` // 0: it never ran, so it holds none
	Up      bool   `json:"up"`
	Commit  string `json:"commit"`
	Subject string `json:"subject"`
	Behind  int    `json:"behind"` // commits main's checkout has that it doesn't
	Ahead   int    `json:"ahead"`
	// Stack is the chain of branches it is stacked on, bottom first, when it
	// sits on another worktree's branch rather than on main.
	Stack      []string      `json:"stack,omitempty"`
	MainBehind bool          `json:"mainBehind,omitempty"`
	OutOfStep  []snapSkew    `json:"outOfStep"`
	Services   []snapService `json:"services"`
	Memory     int64         `json:"memory"`
	Problems   []snapProblem `json:"problems"`
	Strays     []snapStray   `json:"strays,omitempty"`
	Update     *snapUpdate   `json:"update,omitempty"`
	// IdleStopped: its stack isn't running because nobody used it for
	// x-hydra idle_stop, and hydra stopped it.
	IdleStopped *snapIdle `json:"idleStopped,omitempty"`
	// FarBehind: more than behind_warn commits behind main, or merging main
	// conflicts.
	FarBehind *snapFarBehind `json:"farBehind,omitempty"`
	// Pending: not planned yet (the first seconds of a watch).
	Pending   bool      `json:"pending,omitempty"`
	PlannedAt time.Time `json:"plannedAt"`
	GitAt     time.Time `json:"gitAt"`
	Error     string    `json:"error,omitempty"`
}

type snapFarBehind struct {
	Behind    int      `json:"behind"`
	Limit     int      `json:"limit"`               // behind_warn
	Conflicts []string `json:"conflicts,omitempty"` // files merging main into it conflicts in
}

type snapIdle struct {
	At    time.Time `json:"at"`
	After string    `json:"after"` // the idle_stop: "2h"
}

type snapshot struct {
	Version   int            `json:"version"`
	Repo      string         `json:"repo"`    // the main checkout
	Current   string         `json:"current"` // the checkout the command ran in
	BaseRef   string         `json:"baseRef"`
	At        time.Time      `json:"at"`
	Main      *snapMain      `json:"main,omitempty"`
	Worktrees []snapWorktree `json:"worktrees"`
	Error     string         `json:"error,omitempty"`
}

// snapshotter keeps what the slow parts found between snapshots.
type snapshotter struct {
	here gitx.Checkout // where the command runs
	main gitx.Checkout

	mu       sync.Mutex
	ms       *mainState
	subject  string // of main's HEAD commit
	msAt     time.Time
	msErr    error
	mainHead string
	wts      []stack.Worktree // as git lists them, the main checkout left out
	plans    map[string]*wtPlan
	gits     map[string]*wtGit
	busy     map[string]bool   // a slow refresh of this worktree is under way
	gitDirs  map[string]string // by worktree root: its git directory, read once
	changed  chan struct{}     // a slow refresh finished
	mem      func(pid int) int64
	memAt    time.Time
	// What holds the stacks' ports without being part of one, by checkout,
	// looked for every strayAge in the background (at once when once).
	strays     map[string][]stray
	straysAt   time.Time
	straysBusy bool
	once       bool // a single snapshot: nothing is left for the next one
}

// wtPlan is a worktree's plan, and the commits it was made at.
type wtPlan struct {
	ws             *wtState
	err            error
	at             time.Time
	head, mainHead string
}

// wtGit is where a worktree stands against main.
type wtGit struct {
	commit, subject string
	behind, ahead   int
	stack           []string
	update          *snapUpdate
	far             *farBehind
	at              time.Time
	// The commits it was worked out at: the worktree's, main's, and the
	// one of the branch it is stacked on.
	head, mainHead, parentHead string
	index                      indexMark // the worktree's index and merge, as they were
}

// indexMark is what changes, without a commit, when someone stages files,
// begins a merge, resolves it or aborts it: the index file and MERGE_HEAD.
// Two stats, cheap enough for every worktree every second.
type indexMark struct {
	index time.Time
	size  int64
	merge bool
}

func markOf(gitDir string) indexMark {
	var m indexMark
	if gitDir == "" {
		return m
	}
	if fi, err := os.Stat(filepath.Join(gitDir, "index")); err == nil {
		m.index, m.size = fi.ModTime(), fi.Size()
	}
	_, err := os.Stat(filepath.Join(gitDir, "MERGE_HEAD"))
	m.merge = err == nil
	return m
}

// gitDirOf is a worktree's own git directory: .git in the main checkout,
// and for a linked worktree the directory its .git file points to. "" when
// it can't be told.
func gitDirOf(root string) string {
	dotGit := filepath.Join(root, ".git")
	fi, err := os.Stat(dotGit)
	switch {
	case err != nil:
		return ""
	case fi.IsDir():
		return dotGit
	}
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return ""
	}
	dir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
	if !ok {
		return ""
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	return dir
}

// gitDue says whether where a worktree stands has to be worked out again,
// and whether it is urgent: the worktree itself moved (a commit, a merge
// begun or aborted), as opposed to main moving or time passing.
func gitDue(g *wtGit, head, mainHead, parentHead string, mark indexMark, now time.Time) (due, urgent bool) {
	switch {
	case g == nil:
		return true, false
	case g.head != head || g.parentHead != parentHead && g.update != nil:
		return true, true
	case g.index != mark && now.Sub(g.at) >= gitIndexGap:
		return true, true
	case g.mainHead != mainHead:
		return true, false
	}
	return now.Sub(g.at) > gitAge, false
}

func newSnapshotter(here gitx.Checkout) (*snapshotter, error) {
	mainCo, err := gitx.Find(here.Main)
	if err != nil {
		return nil, err
	}
	return &snapshotter{
		here: here, main: mainCo,
		plans: map[string]*wtPlan{}, gits: map[string]*wtGit{}, busy: map[string]bool{}, gitDirs: map[string]string{},
		changed: make(chan struct{}, 1),
	}, nil
}

// statusJSON is `hydra status --json [--watch]`.
func statusJSON(co gitx.Checkout, watch bool) error {
	// Reading git every few seconds must never get in the way of someone's
	// own git command: no index.lock taken to refresh the index.
	os.Setenv("GIT_OPTIONAL_LOCKS", "0")
	s, err := newSnapshotter(co)
	if err != nil {
		return err
	}
	if !watch {
		s.once = true
		s.refresh(true, true, "")
		return json.NewEncoder(os.Stdout).Encode(s.build())
	}
	return s.watch(os.Stdin, os.Stdout)
}

// watch prints a snapshot every second, and one as soon as a slow part
// comes in. A line on in asks for something:
//
//	refresh         everything again, now
//	refresh ROOT    that worktree's plan and git state again (after a merge, say)
//
// It ends when in closes: whoever reads the snapshots has gone.
func (s *snapshotter) watch(in io.Reader, out io.Writer) error {
	cmds := make(chan string)
	go func() {
		sc := bufio.NewScanner(in)
		for sc.Scan() {
			cmds <- strings.TrimSpace(sc.Text())
		}
		close(cmds)
	}()
	w := bufio.NewWriter(out)
	enc := json.NewEncoder(w)
	emit := func() error {
		if err := enc.Encode(s.build()); err != nil {
			return err
		}
		return w.Flush()
	}
	s.refresh(false, false, "")
	if err := emit(); err != nil {
		return err
	}
	last := time.Now()
	tick := time.NewTicker(snapshotEvery)
	defer tick.Stop()
	for {
		select {
		case cmd, ok := <-cmds:
			if !ok {
				return nil
			}
			if root, ok := strings.CutPrefix(cmd, "refresh"); ok {
				s.refresh(false, true, strings.TrimSpace(root))
			}
		case <-tick.C:
			s.refresh(false, false, "")
		case <-s.changed:
			// Slow parts come in one after the other: show them together,
			// at most a few times a second.
			if time.Since(last) < snapshotEvery/3 {
				continue
			}
		}
		if err := emit(); err != nil {
			return err
		}
		last = time.Now()
	}
}

// refresh brings the cached state up to date: which worktrees there are,
// the main checkout, and the slow parts that are due. wait does the slow
// parts before it returns; otherwise they run in the background. force
// makes them all due, or only the worktree at only.
func (s *snapshotter) refresh(wait, force bool, only string) {
	wts, err := stack.List(s.main.Root)
	if err != nil {
		s.mu.Lock()
		s.msErr = err
		s.mu.Unlock()
		return
	}
	var mainHead string
	var mine []stack.Worktree
	heads := map[string]string{} // branch -> commit
	for _, wt := range wts {
		heads[wt.Branch] = wt.Head
		if wt.Root == s.main.Root {
			mainHead = wt.Head
			continue
		}
		mine = append(mine, wt)
	}
	s.refreshMain(force && only == "", mainHead)

	s.mu.Lock()
	s.wts = mine
	live := map[string]bool{}
	type job struct {
		wt                stack.Worktree
		plan, git, urgent bool
		mark              indexMark
	}
	var jobs []job
	now := time.Now()
	for _, wt := range mine {
		live[wt.Root] = true
		if s.busy[wt.Root] {
			continue
		}
		forced := force && (only == "" || only == wt.Root)
		p, g := s.plans[wt.Root], s.gits[wt.Root]
		age := planAgeStopped
		if p != nil && p.ws != nil && p.ws.Up {
			age = planAgeRunning
		}
		gitDir, ok := s.gitDirs[wt.Root]
		if !ok {
			gitDir = gitDirOf(wt.Root)
			s.gitDirs[wt.Root] = gitDir
		}
		j := job{wt: wt, mark: markOf(gitDir)}
		parentHead := ""
		if g != nil && g.update != nil {
			parentHead = heads[g.update.Parent]
		}
		j.git, j.urgent = gitDue(g, wt.Head, mainHead, parentHead, j.mark, now)
		j.git = j.git || forced
		j.plan = forced || p == nil || p.head != wt.Head || p.mainHead != mainHead || now.Sub(p.at) > age
		j.urgent = j.urgent || forced || p != nil && p.head != wt.Head
		if j.plan || j.git {
			s.busy[wt.Root] = true
			jobs = append(jobs, j)
		}
	}
	for root := range s.plans {
		if !live[root] {
			delete(s.plans, root) // the worktree was removed
			delete(s.gits, root)
			delete(s.gitDirs, root)
		}
	}
	var base, mainRoot string
	limit := 0
	if s.ms != nil {
		base, limit = s.ms.target.stack.Hydra.BaseRef, s.ms.target.stack.Hydra.BehindWarn
	}
	mainRoot = s.main.Root
	s.mu.Unlock()

	if len(jobs) == 0 {
		return
	}
	// The worktrees that moved first: they are the ones someone is looking at.
	sort.SliceStable(jobs, func(a, b int) bool { return jobs[a].urgent && !jobs[b].urgent })
	var wg sync.WaitGroup
	wg.Add(len(jobs))
	sem := make(chan struct{}, slowAtOnce)
	run := func(j job) {
		defer wg.Done()
		defer func() { <-sem }()
		var p *wtPlan
		var g *wtGit
		if j.plan {
			p = planWorktree(j.wt, mainHead)
		}
		if j.git && base != "" {
			g = gitState(j.wt, mainRoot, base, mainHead, limit)
			g.parentHead = heads[g.update.Parent]
			g.index = j.mark
		}
		s.mu.Lock()
		if p != nil {
			s.plans[j.wt.Root] = p
		}
		if g != nil {
			s.gits[j.wt.Root] = g
		}
		delete(s.busy, j.wt.Root)
		s.mu.Unlock()
		select {
		case s.changed <- struct{}{}:
		default:
		}
	}
	// In order: a slot goes to the next job only when one is free.
	dispatch := func() {
		for _, j := range jobs {
			sem <- struct{}{}
			go run(j)
		}
	}
	if wait {
		dispatch()
		wg.Wait()
		return
	}
	go dispatch()
}

// refreshMain reads the main checkout again: all of it when that is due
// (its HEAD moved, its stack came up or went down, or it is a few seconds
// old), and otherwise only what its process-compose runs.
func (s *snapshotter) refreshMain(force bool, head string) {
	s.mu.Lock()
	ms, at, last := s.ms, s.msAt, s.mainHead
	s.mu.Unlock()
	full := force || ms == nil || head != last || time.Since(at) > mainAge
	if !full {
		wasUp := ms.Up
		cp := *ms // the old one may be in a snapshot being built
		if err := cp.refresh(); err != nil || cp.Up != wasUp {
			full = true
		} else {
			ms = &cp
		}
	}
	var err error
	subject := ""
	if full {
		ms, err = loadMain(s.main)
		at = time.Now()
		subject, _ = gitOut(s.main.Root, "log", "-1", "--format=%s")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msErr = err
	if err == nil {
		s.ms, s.msAt, s.mainHead = ms, at, head
		if full {
			s.subject = subject
		}
	}
}

// planWorktree plans one worktree, which is the slow part of its state.
func planWorktree(wt stack.Worktree, mainHead string) *wtPlan {
	p := &wtPlan{at: time.Now(), head: wt.Head, mainHead: mainHead}
	wco, err := gitx.Find(wt.Root)
	if err != nil {
		p.err = err
		return p
	}
	p.ws, p.err = loadWorktree(wco)
	return p
}

// gitState works out where a worktree stands against main, and what
// `hydra update` would do to it.
func gitState(wt stack.Worktree, mainRoot, base, mainHead string, limit int) *wtGit {
	g := &wtGit{at: time.Now(), head: wt.Head, mainHead: mainHead}
	if out, err := gitOut(wt.Root, "log", "-1", "--abbrev=9", "--format=%h%x09%s"); err == nil {
		g.commit, g.subject, _ = strings.Cut(out, "\t")
	}
	if out, err := gitOut(wt.Root, "rev-list", "--left-right", "--count", mainHead+"...HEAD"); err == nil {
		if f := strings.Fields(out); len(f) == 2 {
			g.behind, _ = strconv.Atoi(f[0])
			g.ahead, _ = strconv.Atoi(f[1])
		}
	}
	// Cached by the pair of commits (mainConflicts): a look every two
	// minutes for uncommitted work doesn't merge again.
	g.far = farBehindOf(wt.Root, wt.Head, mainHead, g.behind, limit)
	u := &wtUpdate{root: wt.Root, name: wt.Branch}
	u.inspect(nil, base)
	u.decide(mainRoot, base, mainHead, true)
	g.update = &snapUpdate{Parent: u.parent, Action: u.action, Note: u.note, Hand: u.hand}
	for _, line := range u.detail {
		if f, ok := strings.CutPrefix(line, "conflict: "); ok {
			g.update.Conflicts = append(g.update.Conflicts, f)
		}
	}
	if u.depth > 1 {
		g.stack = stackOf(wt.Root, base)
	}
	return g
}

// stackOf names the branches a worktree is stacked on, bottom first, and
// its own last.
func stackOf(root, base string) []string {
	chain, _, err := stack.Chain(root, stack.Options{
		BaseRef: base,
		ParentOverride: func(root string) string {
			lf, _ := readLocal(filepath.Join(root, ".hydra.local.yaml"))
			if lf == nil {
				return ""
			}
			return lf.Parent
		},
	})
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range chain {
		out = append(out, l.Name)
	}
	return out
}

// build puts a snapshot together: the main checkout as last read, each
// worktree's cached plan and git state, and every stack's processes as they
// are now.
func (s *snapshotter) build() *snapshot {
	snap := &snapshot{Version: snapshotVersion, Repo: s.main.Root, Current: s.here.Root, At: time.Now(), Worktrees: []snapWorktree{}}
	s.mu.Lock()
	ms, msErr, subject := s.ms, s.msErr, s.subject
	wts := append([]stack.Worktree(nil), s.wts...)
	plans := make([]*wtPlan, len(wts))
	gits := make([]*wtGit, len(wts))
	for i, wt := range wts {
		plans[i], gits[i] = s.plans[wt.Root], s.gits[wt.Root]
	}
	s.mu.Unlock()
	if ms == nil {
		if msErr != nil {
			snap.Error = msErr.Error()
		}
		return snap
	}
	snap.BaseRef = ms.target.stack.Hydra.BaseRef

	// Process states are cheap: ask every worktree's server again.
	var wg sync.WaitGroup
	for i, p := range plans {
		if p == nil || p.ws == nil {
			continue
		}
		wg.Add(1)
		go func(i int, p *wtPlan) {
			defer wg.Done()
			ws := *p.ws // a copy: the cached plan is shared with other snapshots
			ws.refresh()
			cp := *p
			cp.ws = &ws
			plans[i] = &cp
		}(i, p)
	}
	wg.Wait()

	// The memory of every process tree costs a ps of the machine.
	s.mu.Lock()
	if s.mem == nil || time.Since(s.memAt) > memoryAge {
		s.mem, s.memAt = processTrees(), time.Now()
	}
	mem := s.mem
	s.mu.Unlock()
	strays := s.straysOf(ms, plans)
	snap.Main = snapMainOf(ms, mem)
	snap.Main.Subject = subject
	addStrays(&snap.Main.Strays, &snap.Main.Problems, strays[ms.target.checkout.Root])
	for i, wt := range wts {
		w := snapWorktreeOf(ms, wt, plans[i], gits[i], mem)
		addStrays(&w.Strays, &w.Problems, strays[wt.Root])
		snap.Worktrees = append(snap.Worktrees, w)
	}
	return snap
}

// straysOf returns what was last found holding the stacks' ports, and looks
// again in the background when that is due: an lsof and a ps of the
// machine are too much to run every second.
func (s *snapshotter) straysOf(ms *mainState, plans []*wtPlan) map[string][]stray {
	s.mu.Lock()
	due := !s.straysBusy && (s.strays == nil || time.Since(s.straysAt) > strayAge)
	if due {
		s.straysBusy = true
	}
	have := s.strays
	s.mu.Unlock()
	if !due {
		return have
	}
	want := map[int]portUse{}
	mainPorts(want, ms)
	for _, p := range plans {
		if p != nil && p.ws != nil {
			worktreePorts(want, p.ws)
		}
	}
	look := func() map[string][]stray {
		found := map[string][]stray{}
		for _, st := range findStrays(want) {
			found[st.Root] = append(found[st.Root], st)
		}
		s.mu.Lock()
		s.strays, s.straysAt, s.straysBusy = found, time.Now(), false
		s.mu.Unlock()
		return found
	}
	if s.once {
		return look()
	}
	go func() {
		look()
		select {
		case s.changed <- struct{}{}:
		default:
		}
	}()
	return have
}

func addStrays(into *[]snapStray, problems *[]snapProblem, list []stray) {
	for _, st := range list {
		*into = append(*into, snapStray{Port: st.Port, Service: st.Service, Pid: st.Pid, Command: st.Cmd, Dir: st.Dir, DirGone: st.DirGone})
	}
	*problems = append(*problems, snapProblems(strayProblems(list))...)
}

// processTrees returns, for every process on the machine, the memory it and
// its descendants hold: process-compose reports a process's own, which for
// `pnpm dev` is the wrapper's and not the server's.
func processTrees() func(pid int) int64 {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,rss=").Output()
	if err != nil {
		return func(int) int64 { return 0 }
	}
	kids := map[int][]int{}
	rss := map[int]int64{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		kb, _ := strconv.ParseInt(f[2], 10, 64)
		kids[ppid] = append(kids[ppid], pid)
		rss[pid] = kb * 1024
	}
	return func(pid int) int64 {
		if pid <= 0 {
			return 0
		}
		var total int64
		todo := []int{pid}
		seen := map[int]bool{}
		for len(todo) > 0 {
			p := todo[len(todo)-1]
			todo = todo[:len(todo)-1]
			if seen[p] {
				continue
			}
			seen[p] = true
			total += rss[p]
			todo = append(todo, kids[p]...)
		}
		return total
	}
}

func healthName(h pc.Health) string {
	return [...]string{"up", "done", "starting", "off", "failed"}[h]
}

func snapProblems(ps []problem) []snapProblem {
	out := make([]snapProblem, 0, len(ps))
	for _, p := range ps {
		out = append(out, snapProblem{Blocks: p.Blocks, Passing: p.Passing, Text: p.Text, Fix: p.Fix, Run: runnableFix(p.Fix)})
	}
	return out
}

// runnableFix is a problem's fix as a command, when it is a hydra command
// (and a comment, after three spaces, is left out): "hydra update",
// "cd /repo/main && hydra process restart api". Advice is not.
func runnableFix(fix string) string {
	cmd, _, _ := strings.Cut(fix, "   ")
	cmd = strings.TrimSpace(cmd)
	switch {
	case strings.HasPrefix(cmd, "hydra "):
		return cmd
	case strings.HasPrefix(cmd, "cd ") && strings.Contains(cmd, " && hydra "):
		return cmd
	}
	return ""
}

// serviceURL is a link for a service's port when what it serves is HTTP.
func serviceURL(ms *mainState, name string, port int) string {
	s := ms.target.stack.Services[name]
	if s == nil || port == 0 || !strings.HasPrefix(s.URL, "http") {
		return ""
	}
	return "http://" + net.JoinHostPort(browserHost(ms.target.stack.Hydra.Host), strconv.Itoa(port))
}

// browserHost is the name to put in a link a person opens. Services are
// wired to each other by loopback address (a name can resolve to the other
// address family than the one a service listens on), but a browser is
// better off with "localhost": dev servers treat a page opened by IP as
// another origin, and Next.js then refuses it its hot-reload socket.
func browserHost(host string) string {
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return "localhost"
	}
	return host
}

func snapMainOf(ms *mainState, mem func(int) int64) *snapMain {
	m := &snapMain{
		Root: ms.target.checkout.Root, Branch: ms.Branch, Commit: gitx.Short(ms.Head), Behind: ms.Behind, Dirty: len(ms.Dirty),
		Up: ms.Up, Where: ms.Client.Where, Processes: []snapService{},
	}
	problems := mainProblems(ms)
	if ms.Up {
		problems = append(problems, failing(ms, nil)...)
	}
	problems = append(problems, goneProblems(ms.target.checkout)...)
	m.Problems = snapProblems(problems)
	for _, name := range sortedProcs(ms.Procs) {
		p := ms.Procs[name]
		svc := snapService{Name: name, Runs: "here", State: p.Describe(), Health: healthName(p.Health())}
		if s := ms.target.stack.Services[name]; s != nil {
			svc.Port = s.Port
			svc.URL = serviceURL(ms, name, s.Port)
			svc.Address = ms.target.stack.HostsURL(s, "")
		}
		if p.IsRunning {
			svc.Memory = mem(p.Pid)
			m.Memory += svc.Memory
		}
		m.Processes = append(m.Processes, svc)
	}
	// A stack that isn't running still says what it would run, so the
	// dashboard can show main's services while it is down.
	if !ms.Up && len(ms.Procs) == 0 {
		for _, name := range ms.target.stack.Names() {
			s := ms.target.stack.Services[name]
			m.Processes = append(m.Processes, snapService{
				Name: name, Runs: "here", State: "not running", Health: "off",
				Port: s.Port, URL: serviceURL(ms, name, s.Port), Address: ms.target.stack.HostsURL(s, ""),
			})
		}
	}
	return m
}

func snapWorktreeOf(ms *mainState, wt stack.Worktree, p *wtPlan, g *wtGit, mem func(int) int64) snapWorktree {
	w := snapWorktree{
		Root: wt.Root, Dir: filepath.Base(wt.Root), Branch: wt.Branch,
		OutOfStep: []snapSkew{}, Services: []snapService{}, Problems: []snapProblem{},
	}
	if g != nil {
		w.Commit, w.Subject, w.Behind, w.Ahead, w.Stack, w.Update, w.GitAt = g.commit, g.subject, g.behind, g.ahead, g.stack, g.update, g.at
	}
	if p == nil {
		w.Pending = true
		return w
	}
	w.PlannedAt = p.at
	if p.err != nil || p.ws == nil {
		if p.err != nil {
			w.Error = p.err.Error()
		}
		return w
	}
	ws := p.ws
	if g != nil && g.far != nil {
		ws.Far = g.far
		w.FarBehind = &snapFarBehind{Behind: g.far.Behind, Limit: g.far.Limit, Conflicts: g.far.Conflicts}
	}
	w.Slot, w.Up, w.MainBehind = ws.Slot, ws.Up, ws.Plan.MainBehind
	if n := readIdleNote(wt.Root); n != nil && !ws.Up {
		w.IdleStopped = &snapIdle{At: n.At, After: n.After}
	}
	for _, k := range ws.Plan.OutOfStep {
		w.OutOfStep = append(w.OutOfStep, snapSkew{Service: k.Service, Why: k.Why, Files: k.Files})
	}
	w.Problems = snapProblems(worktreeProblems(ms, ws, ws.Up))

	used := uses(ws.target.stack, ws.Plan)
	for _, row := range ws.Plan.Services {
		svc := snapService{Name: row.Name, Port: row.Port, Why: row.Why}
		switch {
		case row.Local:
			svc.Runs, svc.Off = "here", row.Disabled
			svc.State, svc.Health = "not started", "off"
			if proc, ok := ws.Procs[row.Name]; ok {
				svc.State, svc.Health = proc.Describe(), healthName(proc.Health())
				if proc.IsRunning {
					svc.Memory = mem(proc.Pid)
					w.Memory += svc.Memory
				}
			} else if ws.Up && !row.Disabled {
				svc.State, svc.Health = "not in the project yet", "starting"
			}
			svc.URL = serviceURL(ms, row.Source, row.Port)
		default:
			svc.Runs = "main"
			if row.Routed {
				svc.Runs = "routed"
				svc.URL = serviceURL(ms, row.Name, row.Port)
			}
			_, svc.Used = used[row.Name]
			svc.State, svc.Health = "main is down", "unknown"
			if ms.Up {
				svc.State, svc.Health = "not in main's project", "unknown"
				if proc, ok := ms.Procs[row.Name]; ok {
					svc.State, svc.Health = proc.Describe(), healthName(proc.Health())
				}
			}
		}
		if label := config.HostLabel(wt.Root); label != "" {
			// Stable for as long as the worktree exists, slot or not.
			svc.Address = ws.target.stack.HostsURL(ws.target.stack.Services[row.Source], label)
		}
		if ws.Slot == 0 && svc.Runs != "main" {
			// It never ran, so it has no slot and no ports yet: the plan
			// used the first slot's only to have something to plan with.
			svc.Port, svc.URL = 0, ""
		}
		w.Services = append(w.Services, svc)
	}
	return w
}
