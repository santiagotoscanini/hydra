package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/pc"
	"github.com/santiagotoscanini/hydra/internal/plan"
	"github.com/santiagotoscanini/hydra/internal/render"
)

// A process-compose that is killed, or crashes, doesn't take its processes
// with it: an api goes on holding its port, a worker goes on taking work,
// and `next dev` is two processes, of which the child holds the port and the
// parent a lock on the project. Nothing looks after them any more, and the
// next start of the stack would fail on the ports, or run a second worker
// beside the old one.
//
// hydra stops them, and knows them in two ways.
//
// By their pids. hydra's own process in a stack, the watcher in a worktree
// and the router in main, asks process-compose every two seconds what it
// runs and writes that down (runningFile). When process-compose is gone it
// stops the ones that are still there and without a parent, then exits
// itself (buryOrphans). When it was killed as well, the note is still there:
// the next `hydra up`, a `hydra down`, and the cleanup after a removed
// worktree go by it (orphansOf).
//
// By their port, for a stack nothing wrote a note for (a main checkout
// without a routed service has no router), or a service restarted in the
// last two seconds. Four things together:
//
//   - one of its processes listens on a port of the service that is about to
//     start, in this slot;
//   - its environment carries HYDRA_SLOT and HYDRA_SERVICE for that slot and
//     that service: hydra gives both to every process it renders, and what
//     those start inherits them;
//   - its top process has no parent any more;
//   - it runs from this checkout.
//
// That is the service's own earlier copy. It is stopped whole before the
// service starts: by `hydra up`, and by `hydra process restart` and `start`.
//
// The marks alone are never enough. They also fit what a step of the stack
// leaves running on purpose, which is orphaned and runs from the checkout
// too: the Docker VM a step started, a build tool's daemon. None of those is
// a pid process-compose named, and none listens on the port of the service
// whose marks it carries. A service that does leave such a thing listening
// says so with `is_daemon`, which all of this leaves alone.

// leftover is one orphaned process tree.
type leftover struct {
	Service string
	Pid     int    // its top process, the one without a parent
	Cmd     string // that process's program
	Tree    []int  // every process in it, top first
}

// procInfo is one process of this machine. Slot and Service are hydra's
// marks, "" when the process carries none or its environment can't be read.
// A program that renames itself (next-server does) can't be read either:
// its parent still can.
type procInfo struct {
	Pid, Ppid int
	Cmd       string
	Slot      string
	Service   string
}

// processTable lists this machine's processes. Replaced in tests.
var processTable = listProcesses

// workdir returns a process's working directory, "" when it can't be read.
// Replaced in tests.
var workdir = processDir

// listProcesses reads the processes and their environment: from /proc where
// there is one, from ps elsewhere (macOS).
func listProcesses() map[int]procInfo {
	if entries, err := os.ReadDir("/proc/self"); err == nil && len(entries) > 0 {
		return processesFromProc()
	}
	return processesFromPS(psEnv())
}

func processesFromProc() map[int]procInfo {
	dirs, _ := os.ReadDir("/proc")
	out := map[int]procInfo{}
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", d.Name(), "stat"))
		if err != nil {
			continue
		}
		env, _ := os.ReadFile(filepath.Join("/proc", d.Name(), "environ")) // not another user's
		if p, ok := processFromProcFiles(pid, env, stat); ok {
			out[pid] = p
		}
	}
	// Where a session manager adopts orphans in place of pid 1 (systemd's
	// user instance does), its child is as parentless as init's.
	for pid, p := range out {
		if parent, ok := out[p.Ppid]; ok && p.Ppid != 1 && parent.Cmd == "systemd" {
			p.Ppid = 1
			out[pid] = p
		}
	}
	return out
}

// processFromProcFiles reads one process from its /proc files: environ, the
// environment it was started with, NUL-separated, and stat, which is
// "pid (comm) state ppid ..." where comm may hold spaces and brackets.
func processFromProcFiles(pid int, environ, stat []byte) (procInfo, bool) {
	p := procInfo{Pid: pid}
	open, shut := strings.IndexByte(string(stat), '('), strings.LastIndexByte(string(stat), ')')
	if open < 0 || shut < open {
		return p, false
	}
	p.Cmd = string(stat[open+1 : shut])
	f := strings.Fields(string(stat[shut+1:]))
	if len(f) < 2 {
		return p, false
	}
	var err error
	if p.Ppid, err = strconv.Atoi(f[1]); err != nil {
		return p, false
	}
	for _, kv := range strings.Split(string(environ), "\x00") {
		switch k, v, _ := strings.Cut(kv, "="); k {
		case "HYDRA_SLOT":
			p.Slot = v
		case "HYDRA_SERVICE":
			p.Service = v
		}
	}
	if p.Slot == "" || p.Service == "" {
		p.Slot, p.Service = "", "" // one mark alone is nobody's
	}
	return p, true
}

// psEnv is every process with its environment after its command line, the
// way BSD ps prints it with -E.
func psEnv() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "ps", "-axEww", "-o", "pid=,ppid=,command=").Output()
	return string(out)
}

var (
	slotMark    = regexp.MustCompile(`(?:^| )HYDRA_SLOT=(\d+)(?: |$)`)
	serviceMark = regexp.MustCompile(`(?:^| )HYDRA_SERVICE=(\S+)(?: |$)`)
)

func processesFromPS(listing string) map[int]procInfo {
	out := map[int]procInfo{}
	for _, line := range strings.Split(listing, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		p := procInfo{Pid: pid, Ppid: ppid, Cmd: filepath.Base(f[2])}
		if slot, service := slotMark.FindStringSubmatch(line), serviceMark.FindStringSubmatch(line); slot != nil && service != nil {
			p.Slot, p.Service = slot[1], service[1]
		}
		out[pid] = p
	}
	return out
}

func processDir(pid int) string {
	if dir, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd")); err == nil {
		return dir
	}
	for _, line := range strings.Split(lsof("-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn"), "\n") {
		if strings.HasPrefix(line, "n") {
			return line[1:]
		}
	}
	return ""
}

// listeners maps each TCP port someone listens on to the processes that do.
// Replaced in tests.
var listeners = listeningProcesses

func listeningProcesses() map[int][]int {
	out := map[int][]int{}
	pid := 0
	for _, line := range strings.Split(lsof("-nP", "-iTCP", "-sTCP:LISTEN", "-Fpn"), "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			pid, _ = strconv.Atoi(line[1:])
		case strings.HasPrefix(line, "n") && pid != 0:
			if i := strings.LastIndexByte(line, ':'); i >= 0 {
				if port, err := strconv.Atoi(line[i+1:]); err == nil && !containsInt(out[port], pid) {
					out[port] = append(out[port], pid)
				}
			}
		}
	}
	return out
}

func containsInt(list []int, n int) bool {
	for _, v := range list {
		if v == n {
			return true
		}
	}
	return false
}

// treeOf lists a process and everything below it, top first.
func treeOf(table map[int]procInfo, top int) []int {
	children := map[int][]int{}
	for _, p := range table {
		children[p.Ppid] = append(children[p.Ppid], p.Pid)
	}
	var tree []int
	seen := map[int]bool{}
	for queue := []int{top}; len(queue) > 0; queue = queue[1:] {
		if seen[queue[0]] {
			continue
		}
		seen[queue[0]] = true
		tree = append(tree, queue[0])
		kids := children[queue[0]]
		sort.Ints(kids)
		queue = append(queue, kids...)
	}
	return tree
}

// leftovers finds, for each service named, the tree an earlier run of this
// slot's stack left holding one of its ports. roots are the directories the
// stack's services run from: another repository's slot of the same number
// has services of the same names, and only where a process runs tells them
// apart.
func leftovers(st *config.Stack, slot int, services []string, roots []string) []leftover {
	// Who holds a port of which service. Nobody, almost always: then the
	// processes aren't even listed.
	held := map[string][]int{} // service -> the processes on its ports
	var all map[int][]int
	found := false
	for _, name := range services {
		svc := st.Services[name]
		if svc == nil || isDaemon(st, name) {
			continue
		}
		for _, port := range st.SlotPorts(svc, slot) {
			if all == nil {
				all = listeners()
			}
			held[name] = append(held[name], all[port]...)
			found = found || len(all[port]) > 0
		}
	}
	if !found {
		return nil
	}

	table := processTable()
	var out []leftover
	seen := map[int]bool{}
	for _, name := range services {
		for _, pid := range held[name] {
			// Up to the process that has no parent: the one process-compose
			// started, if this is the service's own copy. The process on the
			// port is often a child, and one whose marks can't be read.
			top, ok := table[pid]
			for hops := 0; ok && top.Ppid != 1 && hops < 64; hops++ {
				top, ok = table[top.Ppid]
			}
			if !ok || top.Ppid != 1 || seen[top.Pid] {
				continue // looked after by something
			}
			if top.Slot != strconv.Itoa(slot) || top.Service != name || !under(workdir(top.Pid), roots) {
				continue // not this service's own copy, or not this repository's
			}
			seen[top.Pid] = true
			out = append(out, leftover{Service: name, Pid: top.Pid, Cmd: top.Cmd, Tree: treeOf(table, top.Pid)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Pid < out[j].Pid
	})
	return out
}

// roster is what a process-compose runs, name -> pid, and when it said so
// (for one read from a note: when the note was written).
type roster struct {
	pids map[string]int
	at   time.Time
}

// orphansOf finds what a process-compose that is gone left running: of the
// processes it had started, the ones that are still there and have no
// parent now. Unlike leftovers, this needs no port to tell by: the pids are
// the ones process-compose itself named.
//
// A roster can be old: a note is read days later, and a watcher keeps the
// last answer of a process-compose that stopped answering. By then a number
// can belong to another program. So a process counts only if it was started
// before the roster was taken: it then had that number when the roster was
// taken, which makes it the one the roster names. And a process whose marks
// can be read has to carry this slot's and its service's.
func orphansOf(r roster, slot int) []leftover {
	there := map[string]int{}
	for name, pid := range r.pids {
		if pidAlive(pid) {
			there[name] = pid
		}
	}
	if len(there) == 0 || r.at.IsZero() {
		return nil // the usual case, and it costs no look at the processes
	}
	table := processTable()
	var out []leftover
	for name, pid := range there {
		p, ok := table[pid]
		if !ok || p.Ppid != 1 || (p.Slot != "" && p.Slot != strconv.Itoa(slot)) || (p.Service != "" && p.Service != name) {
			continue
		}
		if born := startedAt(pid); born.IsZero() || born.After(r.at) {
			continue
		}
		out = append(out, leftover{Service: name, Pid: pid, Cmd: p.Cmd, Tree: treeOf(table, pid)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// pidAlive reports whether there is a process with that number. Replaced in
// tests.
var pidAlive = func(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// startedAt is when a process was started, zero when that can't be read.
// Replaced in tests.
var startedAt = processStart

func processStart(pid int) time.Time {
	if stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat")); err == nil {
		system, _ := os.ReadFile("/proc/stat")
		return startFromProc(stat, system)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "LC_ALL=C") // English day and month names
	out, _ := cmd.Output()
	born, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.TrimSpace(string(out)), time.Local)
	if err != nil {
		return time.Time{}
	}
	return born
}

// startFromProc works a process's start out of its /proc/PID/stat, where
// field 22 counts hundredths of a second since boot, and /proc/stat, whose
// btime line is when that was.
func startFromProc(stat, system []byte) time.Time {
	shut := strings.LastIndexByte(string(stat), ')')
	if shut < 0 {
		return time.Time{}
	}
	f := strings.Fields(string(stat[shut+1:])) // f[0] is field 3
	if len(f) < 20 {
		return time.Time{}
	}
	ticks, err := strconv.ParseInt(f[19], 10, 64)
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(system), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			if boot, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64); err == nil {
				return time.Unix(boot, 0).Add(time.Duration(ticks) * 10 * time.Millisecond)
			}
		}
	}
	return time.Time{}
}

// runningFile is where hydra's own process in the stack at root writes down
// what process-compose runs there. It sits beside the stack's socket, so it
// outlives the checkout's folder. It goes when the stack is stopped the
// regular way (forgetOnStop), when a later command has stopped everything it
// names (stopRemains), and when the slot is freed.
func runningFile(root string) string {
	return strings.TrimSuffix(socketFor(root), ".sock") + ".pids"
}

// remember writes pids to root's runningFile.
func remember(root string, pids map[string]int) {
	data, err := json.Marshal(pids)
	if err != nil {
		return
	}
	path := runningFile(root)
	if os.WriteFile(path+".new", data, 0o644) == nil {
		os.Rename(path+".new", path) // whole or not at all
	}
}

// remembered reads root's runningFile: an empty roster when there is none.
func remembered(root string) roster {
	path := runningFile(root)
	info, err := os.Stat(path)
	if err != nil {
		return roster{}
	}
	data, err := os.ReadFile(path)
	var pids map[string]int
	if err != nil || json.Unmarshal(data, &pids) != nil {
		return roster{}
	}
	return roster{pids: pids, at: info.ModTime()}
}

// leftBehind is everything an earlier run of the stack at root left
// running: what its watcher wrote down and is still there, and the trees on
// the ports of the services named.
func leftBehind(st *config.Stack, root string, slot int, services, roots []string) []leftover {
	found := orphansOf(remembered(root), slot)
	seen := map[int]bool{}
	for _, l := range found {
		seen[l.Pid] = true
	}
	for _, l := range leftovers(st, slot, services, roots) {
		if !seen[l.Pid] {
			found = append(found, l)
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].Service != found[j].Service {
			return found[i].Service < found[j].Service
		}
		return found[i].Pid < found[j].Pid
	})
	return found
}

func under(dir string, roots []string) bool {
	for _, root := range roots {
		if dir != "" && root != "" && inside(dir, root) {
			return true
		}
	}
	return false
}

// leftoverGrace is how long a leftover gets to end after being asked to.
var leftoverGrace = 5 * time.Second

// stopLeftovers ends each tree: it asks every process in it to stop, and
// kills what is still there after leftoverGrace. It returns the trees that
// are gone.
func stopLeftovers(list []leftover) []leftover {
	signal := func(sig syscall.Signal) {
		for _, l := range list {
			for _, pid := range l.Tree {
				syscall.Kill(pid, sig)
			}
		}
	}
	alive := func(l leftover) bool {
		for _, pid := range l.Tree {
			if syscall.Kill(pid, 0) == nil {
				return true
			}
		}
		return false
	}
	anyAlive := func() bool {
		for _, l := range list {
			if alive(l) {
				return true
			}
		}
		return false
	}
	if len(list) == 0 {
		return nil
	}
	signal(syscall.SIGTERM)
	for until := time.Now().Add(leftoverGrace); anyAlive() && time.Now().Before(until); {
		time.Sleep(50 * time.Millisecond)
	}
	if anyAlive() {
		signal(syscall.SIGKILL)
		for until := time.Now().Add(2 * time.Second); anyAlive() && time.Now().Before(until); {
			time.Sleep(50 * time.Millisecond)
		}
	}
	var gone []leftover
	for _, l := range list {
		if !alive(l) {
			gone = append(gone, l)
		}
	}
	return gone
}

// stopAndSay stops each tree and says so: one line for all that stopped,
// one for each that didn't. It reports whether there was any.
func stopAndSay(found []leftover, out io.Writer) bool {
	gone := map[int]bool{}
	for _, l := range stopLeftovers(found) {
		gone[l.Pid] = true
	}
	var stopped []leftover
	for _, l := range found {
		if gone[l.Pid] {
			stopped = append(stopped, l)
			continue
		}
		fmt.Fprintf(out, "hydra: warning: an earlier run of this stack left %s running (%s, pid %d), and it didn't stop: kill -9 %d\n", l.Service, l.Cmd, l.Pid, l.Pid)
	}
	if len(stopped) > 0 {
		fmt.Fprintf(out, "hydra: stopped what an earlier run of this stack left running: %s\n", leftoverNames(stopped, true))
	}
	return len(found) > 0
}

// clearLeftoversOf stops what an earlier run left, for a stack that is
// about to start: every service of the plan, from wherever the plan runs it.
// The ones that don't run here count too: the port of a service this
// worktree takes from main is the one main's router opens for it.
func clearLeftoversOf(t *target, p *plan.Plan, out io.Writer) {
	roots := []string{t.checkout.Root}
	var names []string
	for _, row := range p.Services {
		names = append(names, row.Name)
		if row.Root != "" {
			roots = append(roots, row.Root)
		}
	}
	stopAndSay(leftBehind(t.stack, t.checkout.Root, p.Slot, names, roots), out)
}

// slotOf is the slot a checkout holds: 0 for the main checkout, -1 for a
// worktree without one. It only reads the registry.
func slotOf(co gitx.Checkout) int {
	if co.IsMain() {
		return 0
	}
	for _, e := range mustList(co.CommonDir) {
		if e.Root == co.Root {
			return e.Slot
		}
	}
	return -1
}

// clearLeftoversNamed is for `hydra process restart NAME` and `start NAME`:
// the stack is up, and only what holds a port of the ones asked for is
// looked at. It goes by the stack file and the slot alone, so it costs no
// git.
func clearLeftoversNamed(co gitx.Checkout, file string, names []string, out io.Writer) {
	if file == "" {
		file = defaultFile(co)
	}
	st, err := config.Load(file)
	slot := slotOf(co)
	if err != nil || slot < 0 || len(names) == 0 {
		return
	}
	stopAndSay(leftovers(st, slot, names, []string{co.Root}), out)
}

// leftoverNames is "api (pid 12), web (pid 34)", or "api, web".
func leftoverNames(list []leftover, pids bool) string {
	var parts []string
	for _, l := range list {
		if pids {
			parts = append(parts, fmt.Sprintf("%s (pid %d)", l.Service, l.Pid))
		} else {
			parts = append(parts, l.Service)
		}
	}
	return strings.Join(parts, ", ")
}

// serviceNames lists a stack file's services.
func serviceNames(st *config.Stack) []string {
	var names []string
	for name := range st.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// stopRemains stops what is still running of a stack whose process-compose
// isn't: the processes a killed one left, and with database a worktree's
// own database, which only process-compose's shutdown would have stopped.
// It says what it did to out, and reports whether there was anything.
func stopRemains(co gitx.Checkout, file string, database bool, out io.Writer) bool {
	if file == "" {
		file = defaultFile(co)
	}
	st, err := config.Load(file)
	slot := slotOf(co)
	if err != nil || slot < 0 {
		return false
	}
	did := stopAndSay(leftBehind(st, co.Root, slot, serviceNames(st), []string{co.Root}), out)
	if len(orphansOf(remembered(co.Root), slot)) == 0 {
		os.Remove(runningFile(co.Root)) // nothing it names is left
	}
	if !database || co.IsMain() || !hasKind(st, config.KindSupabase) {
		return did
	}
	n, err := retireSupabase(co.Root, "", false)
	if n > 0 {
		fmt.Fprintln(out, "hydra: stopped this worktree's Supabase stack, which an earlier run of this stack left up")
	}
	if err != nil {
		fmt.Fprintf(out, "hydra: warning: this worktree's Supabase stack may still be up: %v\n", err)
	}
	return did || n > 0 || err != nil
}

// supabaseUp lists the directories whose Supabase stack is up, with one
// call for all of them: nothing when that can't be asked.
func supabaseUp() map[string]bool {
	up := map[string]bool{}
	if _, err := exec.LookPath("supabase"); err != nil {
		return up
	}
	stacks, _ := supabaseStacks(supabaseCLI, "")
	for _, s := range stacks {
		if s.Owner == "reachable" {
			up[s.Root] = true
		}
	}
	return up
}

// startedNow asks a process-compose what it runs. That is what it holds as
// a child ("Running"). A daemon ("Launched") isn't: its pid is the
// launcher's, which ended long ago. And it leaves out hydra's own steps,
// which look after themselves. When process-compose doesn't answer, what it
// said last stands, as of when it said it.
func startedNow(c interface {
	Processes() ([]pc.Process, error)
}, was roster) roster {
	procs, err := c.Processes()
	if err != nil {
		return was
	}
	now := roster{pids: map[string]int{}, at: time.Now()}
	for _, p := range procs {
		if p.Status == "Running" && p.Pid > 0 && !render.Own(p.Name) {
			now.pids[p.Name] = p.Pid
		}
	}
	return now
}

// track is startedNow, written down for whoever comes after this process
// when it changed (see runningFile). The first answer is always written.
func track(root string, c interface {
	Processes() ([]pc.Process, error)
}, was roster) roster {
	now := startedNow(c, was)
	if now.pids != nil && (was.pids == nil || !maps.Equal(now.pids, was.pids)) {
		remember(root, now.pids)
	}
	return now
}

// buryOrphans is the last thing hydra's own process in a stack does when the
// process-compose that ran the stack is gone: it stops what that left
// running. Nothing manages those processes any more. They would hold their
// ports against the next start, and a worker among them would go on taking
// work with the old code next to the new one.
func buryOrphans(started roster, slot int) {
	found := orphansOf(started, slot)
	gone := map[int]bool{}
	for _, l := range stopLeftovers(found) {
		gone[l.Pid] = true
	}
	for _, l := range found {
		if gone[l.Pid] {
			logf("stopped %s (%s, pid %d), which it had left running", l.Service, l.Cmd, l.Pid)
		} else {
			logf("%s (%s, pid %d) is still running and didn't stop: kill -9 %d", l.Service, l.Cmd, l.Pid, l.Pid)
		}
	}
}
