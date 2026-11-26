package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/glob"
	"github.com/santiagotoscanini/hydra/internal/pc"
	"github.com/santiagotoscanini/hydra/internal/plan"
	"github.com/santiagotoscanini/hydra/internal/router"
	"gopkg.in/yaml.v3"
)

// baselineRecord is what hydra remembers about the main checkout's stack:
// the commit its processes were last brought to, and where its
// process-compose server listens. It lives in the shared .git directory, so
// every worktree reads the same one.
type baselineRecord struct {
	Commit  string    `json:"commit"`
	At      time.Time `json:"at"`
	Address string    `json:"address,omitempty"`
	Port    int       `json:"port,omitempty"`
}

const baselineFile = "hydra-baseline.json"

func readBaseline(commonDir string) *baselineRecord {
	data, err := os.ReadFile(filepath.Join(commonDir, baselineFile))
	if err != nil {
		return nil
	}
	var r baselineRecord
	if json.Unmarshal(data, &r) != nil || r.Commit == "" {
		return nil
	}
	return &r
}

func writeBaseline(commonDir string, r baselineRecord) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(commonDir, baselineFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// pcAddress is where process-compose listens when started with these
// arguments in this environment: its --address and --port flags, then
// PC_ADDRESS and PC_PORT_NUM, then its defaults.
func pcAddress(args []string) (string, int) {
	host, port := os.Getenv("PC_ADDRESS"), 0
	if p, err := strconv.Atoi(os.Getenv("PC_PORT_NUM")); err == nil {
		port = p
	}
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		if !inline && i+1 < len(args) {
			value = args[i+1]
		}
		switch name {
		case "--address":
			host = value
		case "-p", "--port":
			if p, err := strconv.Atoi(value); err == nil {
				port = p
			}
		}
	}
	if host == "" {
		host = "localhost"
	}
	if port == 0 {
		port = 8080
	}
	return host, port
}

// mainClient reaches the main checkout's process-compose server: where the
// record says it was started, or where this environment would start one.
func mainClient(commonDir string) *pc.Client {
	if r := readBaseline(commonDir); r != nil && r.Port != 0 {
		return pc.TCP(r.Address, r.Port)
	}
	return pc.TCP(pcAddress(nil))
}

// stale is a process of the main checkout's stack that still runs code from
// before the checkout's files moved.
type stale struct {
	Name  string
	Files []string
}

// mainState is the main checkout and its stack, as they are right now.
type mainState struct {
	target  *target
	Head    string
	Branch  string   // "" when detached
	Dirty   []string // tracked files with uncommitted changes
	Behind  int      // commits its upstream has that it doesn't
	Client  *pc.Client
	Up      bool
	Procs   map[string]pc.Process
	Record  *baselineRecord
	Stale   []stale // nil when the stack runs what is checked out
	Unknown bool    // the stack is up but hydra has no record of its commit
	// ConfigStale: the stack file (or hydra) changed since the stack's
	// file was rendered.
	ConfigStale bool
}

// loadMain reads the main checkout's state. co may be any checkout of the
// repository.
func loadMain(co gitx.Checkout) (*mainState, error) {
	mainCo, err := gitx.Find(co.Main)
	if err != nil {
		return nil, err
	}
	t, err := resolve(mainCo.Root, "")
	if err != nil {
		return nil, err
	}
	ms := &mainState{target: t, Client: mainClient(co.CommonDir), Record: readBaseline(co.CommonDir)}
	if ms.Head, err = gitx.Head(mainCo.Root); err != nil {
		return nil, err
	}
	ms.Branch = gitx.Branch(mainCo.Root)
	if out, err := gitOut(mainCo.Root, "status", "--porcelain", "--untracked-files=no"); err == nil && out != "" {
		for _, line := range strings.Split(out, "\n") {
			if len(line) > 3 {
				ms.Dirty = append(ms.Dirty, line[3:])
			}
		}
	}
	if n, err := gitOut(mainCo.Root, "rev-list", "--count", "HEAD..@{upstream}"); err == nil {
		ms.Behind, _ = strconv.Atoi(n)
	}

	if err := ms.refresh(); err != nil || !ms.Up {
		return ms, err
	}
	switch {
	case ms.Record == nil:
		ms.Unknown = true
	case ms.Record.Commit != ms.Head:
		files, err := gitx.DiffNames(mainCo.Root, ms.Record.Commit, ms.Head)
		if err != nil {
			ms.Unknown = true // the recorded commit is gone (a rewritten main)
			break
		}
		ms.Stale = affected(t.stack, ms.Procs, sourceNames(t.stack), files)
	}
	if want, err := t.render(plan.BaselinePlan(t.stack), ""); err == nil {
		have, _ := os.ReadFile(renderedPath(mainCo.Root))
		ms.ConfigStale = !bytes.Equal(want, have)
	}
	return ms, nil
}

// refresh asks main's process-compose again what runs: the part of the state
// that changes by the second. What git says stays as it was loaded.
func (ms *mainState) refresh() error {
	ms.Procs = nil
	if ms.Up = ms.Client.Alive(); !ms.Up {
		return nil
	}
	procs, err := ms.Client.Processes()
	if err != nil {
		return fmt.Errorf("asking main's process-compose (%s): %w", ms.Client.Where, err)
	}
	ms.Procs = map[string]pc.Process{}
	for _, p := range procs {
		ms.Procs[p.Name] = settle(ms.target.stack, p.Name, p)
	}
	return nil
}

func renderedPath(root string) string {
	return filepath.Join(root, ".process-compose", "hydra.yaml")
}

// startedFromPath is the copy of the stack file the main checkout's running
// stack was rendered from. hydra-router reads it, not the file itself: the
// file can be ahead of what runs until `hydra update`, and the ports the
// router listens on have to be the ones main's processes were given. A
// dispatch port, say, moves with max_slots.
func startedFromPath(root string) string {
	return filepath.Join(root, ".process-compose", "hydra-source.yaml")
}

// sourceNames maps every service to itself: in the main checkout a process
// runs the service of the same name.
func sourceNames(st *config.Stack) map[string]string {
	m := map[string]string{}
	for name := range st.Services {
		m[name] = name
	}
	return m
}

// affected lists the processes whose service's paths take one of the files
// and that have run: an update restarts exactly these. sources maps a
// process name to the service it runs. Left out: processes that never
// started; a process that is off by default (`disabled: true`) unless
// someone has it running right now; and daemons, which hold data
// (restarting a database is never automatic).
func affected(st *config.Stack, procs map[string]pc.Process, sources map[string]string, files []string) []stale {
	var out []stale
	for name, source := range sources {
		s := st.Services[source]
		p, running := procs[name]
		if s == nil || len(s.Paths) == 0 || !running || !mayRestart(st, source, p) {
			continue
		}
		var hit []string
		for _, f := range files {
			if matches(s.Paths, f) || (!s.Shared && matches(st.Hydra.GlobalPaths, f)) {
				hit = append(hit, f)
			}
		}
		if len(hit) > 0 {
			out = append(out, stale{Name: name, Files: hit})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// mayRestart says whether an update may restart a process: it has run, it
// isn't a daemon, and if it is off by default, someone has it running now.
// An update never switches on what is meant to stay off.
func mayRestart(st *config.Stack, source string, p pc.Process) bool {
	if p.Health() == pc.Off || isDaemon(st, source) {
		return false
	}
	return !disabled(st, source) || p.IsRunning
}

// matches applies a paths list (globs, `!` exclusions) to one file.
func matches(patterns []string, file string) bool {
	hit := false
	for _, pat := range patterns {
		if ex, ok := strings.CutPrefix(pat, "!"); ok {
			if glob.Match(ex, file) {
				return false
			}
		} else if !hit && glob.Match(pat, file) {
			hit = true
		}
	}
	return hit
}

func isDaemon(st *config.Stack, name string) bool {
	procs, _ := st.Raw["processes"].(map[string]any)
	proc, _ := procs[name].(map[string]any)
	d, _ := proc["is_daemon"].(bool)
	return d
}

// wtState is one worktree and its stack.
type wtState struct {
	target *target
	Name   string // branch, or the directory when detached
	Slot   int    // 0: it never ran
	Plan   *plan.Plan
	Client *pc.Client
	Up     bool
	Procs  map[string]pc.Process
	// Far is set, by the callers that look (status, the snapshot), when the
	// branch is far behind main or merging main conflicts.
	Far *farBehind
}

// loadWorktree plans a worktree and asks its process-compose what runs. It
// never takes a slot: a worktree that has none is planned as if it had the
// first, which changes ports only.
func loadWorktree(co gitx.Checkout) (*wtState, error) {
	t, err := resolve(co.Root, "")
	if err != nil {
		return nil, err
	}
	ws := &wtState{target: t, Name: gitx.Branch(co.Root), Client: pc.Unix(socketPath(co))}
	if ws.Name == "" {
		ws.Name = filepath.Base(co.Root)
	}
	for _, e := range mustList(co.CommonDir) {
		if e.Root == co.Root {
			ws.Slot = e.Slot
		}
	}
	slot := ws.Slot
	if slot == 0 {
		slot = 1
	}
	o := runningOptions(co.Root)
	o.slot = slot
	if ws.Plan, err = t.build(o); err != nil {
		return nil, err
	}
	return ws, ws.refresh()
}

// refresh asks the worktree's process-compose again what runs, and keeps the
// plan as it was loaded.
func (ws *wtState) refresh() error {
	ws.Procs = nil
	if ws.Up = ws.Client.Alive(); !ws.Up {
		return nil
	}
	procs, err := ws.Client.Processes()
	if err != nil {
		return err
	}
	sources := map[string]string{}
	for _, row := range ws.Plan.Services {
		sources[row.Name] = row.Source
	}
	ws.Procs = map[string]pc.Process{}
	for _, p := range procs {
		ws.Procs[p.Name] = settle(ws.target.stack, sources[p.Name], p)
	}
	return nil
}

// settle corrects one thing in what process-compose reports: a process that
// says when it is ready with a log line (`ready_log_line`) shows no
// readiness at all until that line appears. It is still starting.
func settle(st *config.Stack, source string, p pc.Process) pc.Process {
	if p.IsReady == "-" && p.IsRunning && readyByLog(st, source) {
		p.IsReady = "Not Ready"
	}
	return p
}

func readyByLog(st *config.Stack, source string) bool {
	procs, _ := st.Raw["processes"].(map[string]any)
	proc, _ := procs[source].(map[string]any)
	_, line := proc["ready_log_line"]
	_, probe := proc["readiness_probe"]
	return line && !probe
}

// problem is one thing that keeps a checkout from being trusted.
type problem struct {
	Blocks bool   // an error: `hydra up` won't start on it; otherwise a warning
	Text   string // what is wrong; further lines are detail
	Fix    string // the command that puts it right, if there is one
	// Passing: something is still starting; `hydra status --wait` waits for
	// it to go away.
	Passing bool
}

// uses returns the services this worktree takes from main, each with how:
// the ones a service running here calls or waits for, and a routed service
// it reaches through its own port.
func uses(st *config.Stack, p *plan.Plan) map[string]string {
	local := map[string]bool{}
	for _, row := range p.Services {
		local[row.Name] = row.Local
	}
	out := map[string]string{}
	add := func(svc, how string) {
		if s := st.Services[svc]; s == nil || local[svc] {
			return
		}
		if _, ok := out[svc]; !ok {
			out[svc] = how
		}
	}
	for _, row := range p.Services {
		switch {
		case row.Routed:
			add(row.Name, "this worktree's port for it is :"+strconv.Itoa(row.Port))
		case row.Local && !row.Disabled:
			for _, u := range row.Upstreams {
				if u.Target != plan.Local {
					add(u.Service, row.Name+" here calls it")
				}
			}
			for _, d := range st.Services[row.Source].DependsOn {
				add(d, row.Name+" here waits for it")
			}
		}
	}
	return out
}

// mainProblems is what is wrong with the main checkout's stack, whoever
// looks at it.
func mainProblems(ms *mainState) []problem {
	main := ms.target.checkout.Root
	var out []problem
	if !ms.Up {
		return []problem{{Blocks: true,
			Text: fmt.Sprintf("main's stack isn't running (nothing answers on %s).", ms.Client.Where),
			Fix:  "cd " + shQuote(main) + " && hydra up -D"}}
	}
	if len(ms.Stale) > 0 {
		var lines []string
		for _, s := range ms.Stale {
			lines = append(lines, fmt.Sprintf("  %s: %s", s.Name, someFiles(s.Files)))
		}
		out = append(out, problem{Blocks: true,
			Text: fmt.Sprintf("main's files moved from %s to %s without `hydra update`, so these still run the old code:\n%s",
				gitx.Short(ms.Record.Commit), gitx.Short(ms.Head), strings.Join(lines, "\n")),
			Fix: "hydra update"})
	}
	if ms.Unknown {
		out = append(out, problem{
			Text: "hydra has no record of the commit main's stack runs (it was started by an older hydra, or by process-compose directly).",
			Fix:  "hydra update"})
	}
	if ms.ConfigStale {
		out = append(out, problem{
			Text: "main's stack was started from an older process-compose.yaml (or an older hydra) than the one there now.",
			Fix:  "hydra update"})
	}
	out = append(out, tlsProblems(ms)...)
	out = append(out, hostsProblems(ms)...)
	if p := poolProblem(ms); p != nil {
		out = append(out, *p)
	}
	if p := dbConnsProblem(ms.target.stack); p != nil {
		out = append(out, *p)
	}
	if base := ms.target.stack.Hydra.BaseRef; ms.Branch != base {
		on := "a detached commit"
		if ms.Branch != "" {
			on = "branch " + ms.Branch
		}
		out = append(out, problem{Text: fmt.Sprintf("the main checkout is on %s, not %s: worktrees are compared with what it has checked out.", on, base)})
	}
	if len(ms.Dirty) > 0 {
		out = append(out, problem{Text: "the main checkout has uncommitted changes, which its stack runs: " + someFiles(ms.Dirty)})
	}
	return out
}

// poolProblem says when main's PgBouncer (x-hydra.pool) doesn't accept
// connections: every worktree that takes the database from main connects
// through it.
func poolProblem(ms *mainState) *problem {
	st := ms.target.stack
	s := st.Pooled()
	if s == nil {
		return nil
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort(st.Hydra.Host, strconv.Itoa(s.Pool.Port)), time.Second)
	if err == nil {
		c.Close()
		return nil
	}
	main := ms.target.checkout.Root
	fix := "cd " + shQuote(main) + " && hydra process restart hydra-pgbouncer"
	if _, err := exec.LookPath("pgbouncer"); err != nil {
		fix = "brew install pgbouncer && " + fix
	}
	return &problem{Blocks: true,
		Text: fmt.Sprintf("main's PgBouncer for %s isn't listening on :%d, and worktrees reach main's database through it (x-hydra.pool).", s.Name, s.Pool.Port),
		Fix:  fix}
}

// tlsProblems checks the TLS fronts of main's stack: each has to accept
// connections, and this machine has to trust their certificate, or a browser
// refuses the https address without saying much.
func tlsProblems(ms *mainState) []problem {
	st := ms.target.stack
	if !st.HasTLS() {
		return nil
	}
	main := ms.target.checkout.Root
	var out []problem
	for _, name := range st.Names() {
		s := st.Services[name]
		if s.TLS == nil {
			continue
		}
		addr := net.JoinHostPort(st.Hydra.Host, strconv.Itoa(s.TLS.Port))
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			out = append(out, problem{Blocks: true,
				Text: fmt.Sprintf("main's TLS front for %s isn't listening on :%d (hydra-router opens it).", name, s.TLS.Port),
				Fix:  "cd " + shQuote(main) + " && hydra process restart hydra-router"})
			continue
		}
		c.Close()
	}
	files, err := tlsFilesFor(ms.target)
	if err != nil {
		return append(out, problem{Blocks: true, Text: err.Error()})
	}
	// Whether this machine trusts the certificate only matters once a process
	// is given a front's address.
	used := false
	for _, s := range st.Services {
		for _, u := range s.Upstreams {
			used = used || strings.Contains(u.BaselineValue, "{tls}")
		}
	}
	if _, err := os.Stat(files.Cert); used && err == nil && !trusted(files) {
		pr := problem{Text: "this machine doesn't trust the certificate of main's TLS fronts, so a browser refuses their https addresses.", Fix: "hydra trust"}
		if !files.Own {
			pr.Fix = "trust the CA that signed " + files.Cert + " (mkcert -install, if mkcert made it)"
		}
		out = append(out, pr)
	}
	return out
}

// hostsProblems checks main's hosts ports: hydra's router has to answer on
// each, or the name-based addresses don't work. A note: the stack itself
// runs fine without them.
func hostsProblems(ms *mainState) []problem {
	st := ms.target.stack
	client := &http.Client{Timeout: time.Second}
	var out []problem
	for _, name := range st.Names() {
		s := st.Services[name]
		if s.Hosts == 0 {
			continue
		}
		u := "http://" + net.JoinHostPort(st.Hydra.Host, strconv.Itoa(s.Hosts)) + router.StatusPath
		resp, err := client.Get(u)
		if err == nil {
			var ans struct {
				Router  bool   `json:"router"`
				Service string `json:"service"`
				Hosts   bool   `json:"hosts"`
			}
			ok := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&ans) == nil && ans.Router && ans.Hosts && ans.Service == name
			resp.Body.Close()
			if ok {
				continue
			}
		}
		pr := problem{
			Text: fmt.Sprintf("hydra's router isn't answering on :%d, so %s's addresses by worktree name (<worktree>.localhost:%d) don't work.", s.Hosts, name, s.Hosts),
			Fix:  "cd " + shQuote(ms.target.checkout.Root) + " && hydra process restart hydra-router",
		}
		if err == nil {
			pr.Text = fmt.Sprintf("another program answers on :%d, x-hydra.hosts of %s, so its addresses by worktree name don't work.", s.Hosts, name)
			pr.Fix = "lsof -nP -iTCP:" + strconv.Itoa(s.Hosts) + " -sTCP:LISTEN names it; stop it, or move x-hydra.hosts"
		}
		out = append(out, pr)
	}
	return out
}

// failing lists main's processes that are enabled but not up.
func failing(ms *mainState, only map[string]string) []problem {
	var out []problem
	for _, name := range sortedProcs(ms.Procs) {
		p := ms.Procs[name]
		how, used := only[name]
		if only != nil && !used {
			continue
		}
		suffix := ""
		if used {
			suffix = " (" + how + ")"
		}
		switch p.Health() {
		case pc.Failed:
			main := ms.target.checkout.Root
			tail, lines := logTail(ms.Client, name)
			pr := problem{Blocks: true,
				Text: fmt.Sprintf("main's %s %s%s.%s", name, p.Describe(), suffix, tail),
				Fix:  "cd " + shQuote(main) + " && hydra process restart " + name}
			if h := portTaken(ms.target.stack, name, 0, lines); h != nil {
				text, fix := takenBy(h, ms.target.stack, 0, name, main, "hydra process restart "+name)
				pr.Text += text
				pr.Fix = "cd " + shQuote(main) + " && " + fix
			}
			out = append(out, pr)
		case pc.Starting:
			out = append(out, problem{Passing: true, Text: fmt.Sprintf("main's %s is still %s%s.", name, p.Describe(), suffix)})
		case pc.Off:
			if used && only != nil && !disabled(ms.target.stack, name) {
				out = append(out, problem{Blocks: true,
					Text: fmt.Sprintf("main's %s isn't running (%s)%s.", name, p.Describe(), suffix),
					Fix:  "cd " + shQuote(ms.target.checkout.Root) + " && hydra process start " + name})
			}
		}
	}
	if only != nil {
		for _, name := range sortedKeys(only) {
			if _, ok := ms.Procs[name]; !ok {
				out = append(out, problem{Blocks: true,
					Text: fmt.Sprintf("main's stack has no %s (%s): it was started from an older process-compose.yaml.", name, only[name]),
					Fix:  "hydra update"})
			}
		}
	}
	return out
}

// worktreeProblems is everything that keeps this worktree's stack from
// being what `hydra plan` says. started is false before `hydra up`, when
// nothing can be expected to run here yet.
func worktreeProblems(ms *mainState, ws *wtState, started bool) []problem {
	st, p := ws.target.stack, ws.Plan
	used := uses(st, p)
	var out []problem

	if len(used) > 0 || started {
		for _, mp := range mainProblems(ms) {
			if !ms.Up && len(used) > 0 {
				mp.Text += " This worktree takes from it: " + strings.Join(sortedKeys(used), ", ") + "."
			}
			if len(ms.Stale) > 0 && strings.HasPrefix(mp.Text, "main's files moved") {
				// Only a stale service this worktree uses blocks it.
				mp.Blocks = false
				for _, s := range ms.Stale {
					if _, ok := used[s.Name]; ok {
						mp.Blocks = true
					}
				}
			}
			if pooled := st.Pooled(); pooled != nil && strings.HasPrefix(mp.Text, "main's PgBouncer") {
				_, mp.Blocks = used[pooled.Name]
			}
			out = append(out, mp)
		}
	}
	if ms.Up {
		out = append(out, failing(ms, used)...)
	}

	if pr := skewProblem(st, p); pr != nil {
		out = append(out, *pr)
	}
	if ws.Far != nil {
		out = append(out, ws.Far.problem(st.Hydra.BaseRef))
	}

	if started {
		switch {
		case !ws.Up:
			pr := problem{Blocks: true, Text: "this worktree's stack isn't running.", Fix: "hydra up -D"}
			if n := readIdleNote(ws.target.checkout.Root); n != nil {
				pr.Text = n.text()
			}
			out = append(out, pr)
		default:
			out = append(out, localProblems(ms, ws)...)
		}
	}

	for _, w := range p.Warnings {
		out = append(out, problem{Text: w})
	}
	if len(p.Unclaimed) > 0 {
		out = append(out, problem{
			Text: unclaimedNote(p.Unclaimed),
			Fix:  "if a service uses them, add them to its paths (`hydra up SERVICE` runs one here meanwhile); if none does, add them to x-hydra.ignore"})
	}
	return out
}

// skewProblem says which services this worktree takes from main at another
// version than its branch started from, and which side has to move.
func skewProblem(st *config.Stack, p *plan.Plan) *problem {
	if len(p.OutOfStep) == 0 {
		return nil
	}
	var lines []string
	for _, k := range p.OutOfStep {
		lines = append(lines, fmt.Sprintf("  %s (%s): %s", k.Service, k.Why, fewFiles(k.Files, 2, 60)))
	}
	if p.MainBehind {
		return &problem{Blocks: true, Fix: "hydra update",
			Text: "main is older than the commit this branch started from, and this worktree uses main's copy of:\n" + strings.Join(lines, "\n")}
	}
	return &problem{Blocks: true,
		Fix:  "hydra update --here   (or, for every worktree: hydra update)",
		Text: "this branch is behind main: main changed these since the branch left it, and this worktree uses main's copy:\n" + strings.Join(lines, "\n")}
}

// unclaimedNote names the changed files no service's paths cover.
func unclaimedNote(files []string) string {
	return "changed, but in no service's paths, so nothing runs here for them: " + someFiles(files)
}

// localProblems checks what should run in this worktree against what its
// process-compose reports, and its routed services against the router.
func localProblems(ms *mainState, ws *wtState) []problem {
	var out []problem
	st := ws.target.stack
	// Anything that failed in this worktree's project, the steps hydra adds
	// (a Temporal namespace, say) included: a service that waits for one of
	// those is skipped, and the reason is in the step's output.
	sources := map[string]string{}
	for _, row := range ws.Plan.Services {
		sources[row.Name] = row.Source
	}
	for _, name := range sortedProcs(ws.Procs) {
		if p := ws.Procs[name]; p.Health() == pc.Failed && name != "hydra-watch" {
			tail, lines := logTail(ws.Client, name)
			pr := problem{Blocks: true,
				Text: fmt.Sprintf("%s here %s.%s", name, p.Describe(), tail),
				Fix:  "hydra process restart " + name}
			if h := portTaken(st, sources[name], ws.Plan.Slot, lines); h != nil {
				text, fix := takenBy(h, st, ws.Plan.Slot, name, ws.target.checkout.Root, "hydra process restart "+name)
				pr.Text += text
				pr.Fix = fix
			}
			out = append(out, pr)
		}
	}
	for _, row := range ws.Plan.Services {
		switch {
		case row.Local && !row.Disabled:
			p, ok := ws.Procs[row.Name]
			if !ok {
				out = append(out, problem{Passing: true, Text: row.Name + " should run here but isn't in the running project yet; hydra-watch adds it within seconds."})
				continue
			}
			if src := st.Services[row.Source]; src != nil && src.Kind == config.KindSupabase && p.Health() == pc.Up {
				root := ws.target.checkout.Root
				if row.Root != "" {
					root = row.Root
				}
				if pr := supabaseBranchNote(root, row.Name); pr != nil {
					out = append(out, *pr)
				}
				if pr := migrationsProblem(root, row.Name); pr != nil {
					out = append(out, *pr)
				}
			}
			switch p.Health() {
			case pc.Starting:
				out = append(out, problem{Passing: true, Text: fmt.Sprintf("%s here is still %s.", row.Name, p.Describe())})
			case pc.Off:
				pr := problem{Blocks: true,
					Text: fmt.Sprintf("%s should run here but is %s.", row.Name, p.Describe()),
					Fix:  "hydra process start " + row.Name}
				if p.Status == "Skipped" {
					pr.Text = row.Name + " wasn't started here, because a process it waits for failed."
					pr.Fix = "hydra process start " + row.Name + "   (once what it waits for is fixed)"
				}
				out = append(out, pr)
			}
		case row.Routed && ms.Up:
			if reason := routedReason(st.Hydra.Host, row.Port); reason != "" {
				out = append(out, problem{Blocks: true,
					Text: fmt.Sprintf("%s: main's copy can't serve this worktree on :%d: %s", row.Name, row.Port, reason),
					Fix:  "hydra up " + row.Name + "   (runs a copy here instead)"})
			}
		}
	}
	if w, ok := ws.Procs["hydra-watch"]; !ok || w.Health() != pc.Up {
		out = append(out, problem{Blocks: true,
			Text: "hydra-watch isn't running here, so a change won't start its service.",
			Fix:  "hydra process restart hydra-watch"})
	}
	return out
}

// routedReason asks the router, on a worktree's port for a routed service,
// whether main's copy can serve it. "" means yes.
func routedReason(host string, port int) string {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, strconv.Itoa(port)) + router.StatusPath)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return "main's hydra-router isn't listening there"
		}
		return err.Error()
	}
	defer resp.Body.Close()
	var out struct {
		Router bool   `json:"router"`
		Reason string `json:"reason"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil || !out.Router {
		return "something other than main's hydra-router answers there"
	}
	return out.Reason
}

// logTail is the end of a process's output, indented under a message, and
// the same lines as they came.
func logTail(c *pc.Client, name string) (string, []string) {
	lines, err := c.Logs(name, 10)
	if err != nil || len(lines) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString(" Its last output:")
	for _, l := range lines {
		if r := []rune(l); len(r) > 160 {
			l = string(r[:159]) + "…"
		}
		b.WriteString("\n    | " + l)
	}
	return b.String(), lines
}

// someFiles shows the first few of a list and counts the rest. Long paths
// lose their middle: the top directory and the file name say enough.
func someFiles(files []string) string {
	return fewFiles(files, 3, 72)
}

// oneFile is someFiles for a table cell: one path and a count.
func oneFile(files []string) string {
	return fewFiles(files, 1, 56)
}

func fewFiles(files []string, show, width int) string {
	short := make([]string, 0, show)
	for i, f := range files {
		if i == show {
			break
		}
		if r := []rune(f); len(r) > width && !strings.HasPrefix(f, "(") { // a reason in brackets isn't a path
			head := width/2 - 1
			f = string(r[:head]) + "…" + string(r[len(r)-(width-head-1):])
		}
		short = append(short, f)
	}
	if len(files) <= show {
		return strings.Join(short, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(short, ", "), len(files)-show)
}

func blocking(ps []problem) []problem {
	var out []problem
	for _, p := range ps {
		if p.Blocks {
			out = append(out, p)
		}
	}
	return out
}

func printProblems(w *os.File, ps []problem) {
	logProblems(ps)
	for _, p := range ps {
		mark := "!"
		if p.Blocks {
			mark = "✗"
		}
		fmt.Fprintf(w, "%s %s\n", mark, strings.ReplaceAll(p.Text, "\n", "\n  "))
		if p.Fix != "" {
			fmt.Fprintf(w, "  fix: %s\n", p.Fix)
		}
	}
}

func sortedProcs(m map[string]pc.Process) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func disabled(st *config.Stack, name string) bool {
	procs, _ := st.Raw["processes"].(map[string]any)
	proc, _ := procs[name].(map[string]any)
	d, _ := proc["disabled"].(bool)
	return d
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := gitx.WorkTree(dir, args...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// runningOptions recovers the options a worktree's stack was started with
// (`hydra up web` runs web there too) from the watcher's command line in
// its rendered file, so a later look plans the same thing the watcher does.
func runningOptions(root string) options {
	o := options{slot: -1}
	data, err := os.ReadFile(renderedPath(root))
	if err != nil {
		return o
	}
	var doc struct {
		Processes map[string]struct {
			Command string `yaml:"command"`
		} `yaml:"processes"`
	}
	if yaml.Unmarshal(data, &doc) != nil {
		return o
	}
	args := shSplit(doc.Processes["hydra-watch"].Command)
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--with":
			o.with = append(o.with, args[i+1])
		case "--without":
			o.without = append(o.without, args[i+1])
		case "--route":
			o.routes = append(o.routes, args[i+1])
		}
	}
	return o
}

// shSplit undoes shQuote: words separated by spaces, single-quoted when
// they hold anything else.
func shSplit(s string) []string {
	var out []string
	var cur strings.Builder
	quoted, have := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quoted && c == '\'':
			quoted = false
		case quoted:
			cur.WriteByte(c)
		case c == '\'':
			quoted, have = true, true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			have = true
		case c == ' ':
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}
