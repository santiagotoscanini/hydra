package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/journal"
	"github.com/santiagotoscanini/hydra/internal/pc"
	"github.com/santiagotoscanini/hydra/internal/stack"
)

//go:embed ui.html
var uiPage []byte

// uiPort is where `hydra ui` listens unless told otherwise ("HYDR" on a
// phone keypad). A second repository's page takes the next free port.
const uiPort = 4937

// How long a worktree's plan is shown before it is worked out again. A plan
// costs a walk of the working tree (git has to list untracked files), so the
// page asks for process states every few seconds but for plans far less
// often, and for a worktree that runs nothing less often still.
const (
	planAgeRunning = 10 * time.Second
	planAgeStopped = 90 * time.Second
)

// uiCmd serves the status page: the main checkout, every worktree, what
// each runs and what it takes from main, as a local web page that keeps
// itself up to date.
//
//	hydra ui [--port N] [--no-open]
func uiCmd(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	port := fs.Int("port", uiPort, "")
	noOpen := fs.Bool("no-open", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	co, err := gitx.Find(".")
	if err != nil {
		return err
	}
	journal.Log("start", journal.Fields{"what": "ui"})
	mainCo, err := gitx.Find(co.Main)
	if err != nil {
		return err
	}

	// One page per repository. If this repository's is already up (as a
	// process of the main checkout's stack, say), open that one.
	var ln net.Listener
	for p := *port; p < *port+20; p++ {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(p))
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		if repo := uiRepoAt(addr); repo == mainCo.Root {
			url := "http://" + addr
			fmt.Println("hydra: the status page for this repository is already up at", url)
			if !*noOpen {
				openBrowser(url)
			}
			return nil
		}
	}
	if ln == nil {
		return fmt.Errorf("no free port from %d to %d for the status page; pass --port", *port, *port+19)
	}
	url := "http://" + ln.Addr().String()
	srv := &http.Server{Handler: newUI(mainCo).handler()}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	fmt.Printf("hydra: status page for %s at %s\n", mainCo.Root, url)
	if !*noOpen {
		openBrowser(url)
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// uiRepoAt asks whatever listens on addr which repository's status page it
// is; "" when it isn't one.
func uiRepoAt(addr string) string {
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + addr + "/api/ping")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Hydra bool   `json:"hydra"`
		Repo  string `json:"repo"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil || !out.Hydra {
		return ""
	}
	return out.Repo
}

func openBrowser(url string) {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	exec.Command(opener, url).Start()
}

// What the page gets, as JSON.

type uiProblem struct {
	Blocks  bool   `json:"blocks"`
	Passing bool   `json:"passing,omitempty"`
	Text    string `json:"text"`
	Fix     string `json:"fix,omitempty"`
}

type uiService struct {
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
	Why    string `json:"why,omitempty"`
	Memory int64  `json:"memory,omitempty"` // bytes: the process and its children
}

type uiSkew struct {
	Service string   `json:"service"`
	Why     string   `json:"why"`
	Files   []string `json:"files"`
}

type uiMain struct {
	Root      string      `json:"root"`
	Branch    string      `json:"branch"`
	Commit    string      `json:"commit"`
	Subject   string      `json:"subject"`
	Behind    int         `json:"behind"` // commits its upstream has that it doesn't
	Up        bool        `json:"up"`
	Where     string      `json:"where"`
	Processes []uiService `json:"processes"`
	Memory    int64       `json:"memory"`
	Problems  []uiProblem `json:"problems"`
}

type uiWorktree struct {
	Root       string      `json:"root"`
	Dir        string      `json:"dir"`
	Branch     string      `json:"branch"`
	Slot       int         `json:"slot"`
	Up         bool        `json:"up"`
	Commit     string      `json:"commit"`
	Subject    string      `json:"subject"`
	Behind     int         `json:"behind"`
	Ahead      int         `json:"ahead"`
	MainBehind bool        `json:"mainBehind,omitempty"`
	OutOfStep  []uiSkew    `json:"outOfStep"`
	Services   []uiService `json:"services"`
	Memory     int64       `json:"memory"`
	Problems   []uiProblem `json:"problems"`
	PlannedAt  time.Time   `json:"plannedAt"`
	Error      string      `json:"error,omitempty"`
}

type uiSnapshot struct {
	Repo      string       `json:"repo"`
	At        time.Time    `json:"at"`
	Main      *uiMain      `json:"main,omitempty"`
	Worktrees []uiWorktree `json:"worktrees"`
	Error     string       `json:"error,omitempty"`
}

// ui holds what the page shows between requests.
type ui struct {
	main gitx.Checkout

	mu      sync.Mutex
	snap    *uiSnapshot // the last one built
	plans   map[string]*uiPlan
	working bool // a refresh of plans is under way
}

// uiPlan is the slow part of a worktree's state: its plan and the git facts
// around it.
type uiPlan struct {
	ws      *wtState
	err     error
	commit  string
	subject string
	behind  int
	ahead   int
	at      time.Time
}

func newUI(mainCo gitx.Checkout) *ui {
	return &ui{main: mainCo, plans: map[string]*uiPlan{}}
}

func (u *ui) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"hydra": true, "repo": u.main.Root})
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, u.snapshot(r.URL.Query().Has("refresh")))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(uiPage)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// snapshot returns the state to show. Process states are read on every call
// (at most once a second); plans come from the cache, and the ones that are
// due are worked out in the background, so a call never waits for git to
// walk a dozen working trees. force plans everything again and waits.
func (u *ui) snapshot(force bool) *uiSnapshot {
	u.mu.Lock()
	if !force && u.snap != nil && time.Since(u.snap.At) < time.Second {
		defer u.mu.Unlock()
		return u.snap
	}
	u.mu.Unlock()

	if force {
		u.refreshPlans(true)
	} else {
		u.mu.Lock()
		first := len(u.plans) == 0
		start := !u.working
		if start {
			u.working = true
		}
		u.mu.Unlock()
		switch {
		case first && start:
			u.refreshPlans(false) // nothing to show yet: wait for it
			u.done()
		case start:
			go func() {
				u.refreshPlans(false)
				u.done()
			}()
		}
	}

	snap := u.build()
	u.mu.Lock()
	u.snap = snap
	u.mu.Unlock()
	return snap
}

func (u *ui) done() {
	u.mu.Lock()
	u.working = false
	u.mu.Unlock()
}

// refreshPlans works out again the plans that are due (all of them with
// everything), a few at a time.
func (u *ui) refreshPlans(everything bool) {
	wts, err := stack.List(u.main.Root)
	if err != nil {
		return
	}
	mainHead, _ := gitx.Head(u.main.Root)
	live := map[string]bool{}
	var due []string
	u.mu.Lock()
	for _, wt := range wts {
		if wt.Root == u.main.Root {
			continue
		}
		live[wt.Root] = true
		p := u.plans[wt.Root]
		age := planAgeStopped
		if p != nil && p.ws != nil && p.ws.Up {
			age = planAgeRunning
		}
		if everything || p == nil || time.Since(p.at) > age {
			due = append(due, wt.Root)
		}
	}
	for root := range u.plans {
		if !live[root] {
			delete(u.plans, root) // the worktree was removed
		}
	}
	u.mu.Unlock()

	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, root := range due {
		wg.Add(1)
		go func(root string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			p := planWorktree(root, mainHead)
			u.mu.Lock()
			u.plans[root] = p
			u.mu.Unlock()
		}(root)
	}
	wg.Wait()
}

// planWorktree is the slow part for one worktree.
func planWorktree(root, mainHead string) *uiPlan {
	p := &uiPlan{at: time.Now()}
	wco, err := gitx.Find(root)
	if err != nil {
		p.err = err
		return p
	}
	if out, err := gitOut(root, "log", "-1", "--abbrev=9", "--format=%h%x09%s"); err == nil {
		p.commit, p.subject, _ = strings.Cut(out, "\t")
	}
	if out, err := gitOut(root, "rev-list", "--left-right", "--count", mainHead+"...HEAD"); err == nil {
		if f := strings.Fields(out); len(f) == 2 {
			p.behind, _ = strconv.Atoi(f[0])
			p.ahead, _ = strconv.Atoi(f[1])
		}
	}
	p.ws, p.err = loadWorktree(wco)
	return p
}

// build puts the snapshot together: the main checkout's state now, each
// worktree's cached plan with its processes as they are now.
func (u *ui) build() *uiSnapshot {
	snap := &uiSnapshot{Repo: u.main.Root, At: time.Now(), Worktrees: []uiWorktree{}}
	ms, err := loadMain(u.main)
	if err != nil {
		snap.Error = err.Error()
		return snap
	}
	u.mu.Lock()
	plans := make([]*uiPlan, 0, len(u.plans))
	roots := make([]string, 0, len(u.plans))
	for root := range u.plans {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	for _, root := range roots {
		plans = append(plans, u.plans[root])
	}
	u.mu.Unlock()

	// Process states are cheap: ask every worktree's server again.
	for i, p := range plans {
		if p.ws == nil {
			continue
		}
		ws := *p.ws // a copy: the cached plan is shared with other requests
		ws.Up, ws.Procs = false, nil
		if ws.Client.Alive() {
			if procs, err := ws.Client.Processes(); err == nil {
				ws.Up = true
				sources := map[string]string{}
				for _, row := range ws.Plan.Services {
					sources[row.Name] = row.Source
				}
				ws.Procs = map[string]pc.Process{}
				for _, proc := range procs {
					ws.Procs[proc.Name] = settle(ws.target.stack, sources[proc.Name], proc)
				}
			}
		}
		cp := *p
		cp.ws = &ws
		plans[i] = &cp
	}

	mem := processTrees()
	snap.Main = uiMainOf(ms, mem)
	for i, p := range plans {
		snap.Worktrees = append(snap.Worktrees, uiWorktreeOf(ms, roots[i], p, mem))
	}
	return snap
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

func uiProblems(ps []problem) []uiProblem {
	out := make([]uiProblem, 0, len(ps))
	for _, p := range ps {
		out = append(out, uiProblem{Blocks: p.Blocks, Passing: p.Passing, Text: p.Text, Fix: p.Fix})
	}
	return out
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

func uiMainOf(ms *mainState, mem func(int) int64) *uiMain {
	m := &uiMain{
		Root: ms.target.checkout.Root, Branch: ms.Branch, Commit: gitx.Short(ms.Head), Behind: ms.Behind,
		Up: ms.Up, Where: ms.Client.Where, Processes: []uiService{},
	}
	if out, err := gitOut(m.Root, "log", "-1", "--format=%s"); err == nil {
		m.Subject = out
	}
	problems := mainProblems(ms)
	if ms.Up {
		problems = append(problems, failing(ms, nil)...)
	}
	problems = append(problems, goneProblems(ms.target.checkout)...)
	m.Problems = uiProblems(problems)
	for _, name := range sortedProcs(ms.Procs) {
		p := ms.Procs[name]
		svc := uiService{Name: name, Runs: "here", State: p.Describe(), Health: healthName(p.Health())}
		if s := ms.target.stack.Services[name]; s != nil {
			svc.Port = s.Port
			svc.URL = serviceURL(ms, name, s.Port)
		}
		if p.IsRunning {
			svc.Memory = mem(p.Pid)
			m.Memory += svc.Memory
		}
		m.Processes = append(m.Processes, svc)
	}
	return m
}

func uiWorktreeOf(ms *mainState, root string, p *uiPlan, mem func(int) int64) uiWorktree {
	w := uiWorktree{
		Root: root, Dir: filepath.Base(root), Commit: p.commit, Subject: p.subject, Behind: p.behind, Ahead: p.ahead,
		PlannedAt: p.at, OutOfStep: []uiSkew{}, Services: []uiService{}, Problems: []uiProblem{},
	}
	if p.err != nil || p.ws == nil {
		w.Branch = gitx.Branch(root)
		if p.err != nil {
			w.Error = p.err.Error()
		}
		return w
	}
	ws := p.ws
	w.Branch, w.Slot, w.Up, w.MainBehind = ws.Name, ws.Slot, ws.Up, ws.Plan.MainBehind
	for _, k := range ws.Plan.OutOfStep {
		w.OutOfStep = append(w.OutOfStep, uiSkew{Service: k.Service, Why: k.Why, Files: k.Files})
	}
	w.Problems = uiProblems(worktreeProblems(ms, ws, ws.Up))

	used := uses(ws.target.stack, ws.Plan)
	for _, row := range ws.Plan.Services {
		svc := uiService{Name: row.Name, Port: row.Port, Why: row.Why}
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
		if ws.Slot == 0 && svc.Runs != "main" {
			// It never ran, so it has no slot and no ports yet: the plan
			// used the first slot's only to have something to plan with.
			svc.Port, svc.URL = 0, ""
		}
		w.Services = append(w.Services, svc)
	}
	return w
}
