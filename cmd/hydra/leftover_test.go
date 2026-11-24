package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/pc"
)

const leftoverStack = `
x-hydra: { slot_stride: 200 }
processes:
  colima:
    command: colima start
  supabase:
    command: supabase start
    is_daemon: true
    x-hydra: { shared: true, port: 54321 }
  api:
    command: api
    x-hydra: { port: 8080, paths: ["api/**"] }
  web:
    command: next dev
    x-hydra: { port: 3000, paths: ["web/**"] }
  worker:
    command: worker
    x-hydra: { paths: ["worker/**"] }
`

// machine stands in for the processes of this computer.
type machine struct {
	procs  []procInfo
	ports  map[int][]int
	dirs   map[int]string
	born   map[int]time.Time
	listed int // how often the processes were listed
}

func (m *machine) install(t *testing.T) {
	t.Helper()
	oldTable, oldListeners, oldDir, oldBorn, oldAlive := processTable, listeners, workdir, startedAt, pidAlive
	processTable = func() map[int]procInfo {
		m.listed++
		out := map[int]procInfo{}
		for _, p := range m.procs {
			out[p.Pid] = p
		}
		return out
	}
	listeners = func() map[int][]int { return m.ports }
	workdir = func(pid int) string { return m.dirs[pid] }
	startedAt = func(pid int) time.Time { return m.born[pid] }
	pidAlive = func(pid int) bool {
		for _, p := range m.procs {
			if p.Pid == pid {
				return true
			}
		}
		return false
	}
	t.Cleanup(func() {
		processTable, listeners, workdir, startedAt, pidAlive = oldTable, oldListeners, oldDir, oldBorn, oldAlive
	})
}

func leftoverSt(t *testing.T) *config.Stack {
	t.Helper()
	st, err := config.Parse([]byte(leftoverStack))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func trees(list []leftover) string {
	var out []string
	for _, l := range list {
		var pids []string
		for _, p := range l.Tree {
			pids = append(pids, strconv.Itoa(p))
		}
		out = append(out, l.Service+":"+l.Cmd+":"+strings.Join(pids, "+"))
	}
	return strings.Join(out, " ")
}

// `next dev` after its process-compose was killed, as it looks on a Mac: a
// parent without a parent of its own, the child that holds the port and has
// renamed itself, so that its environment can't be read, and a worker under
// that. All three go, and nothing else does.
func TestALeftoverIsTheServicesOwnOrphanedCopyHoldingItsPort(t *testing.T) {
	st := leftoverSt(t)
	all := []string{"colima", "supabase", "api", "web", "worker"}
	roots := []string{"/wt/a"}
	m := &machine{
		procs: []procInfo{
			{Pid: 100, Ppid: 1, Cmd: "node", Slot: "4", Service: "web"},   // next dev
			{Pid: 101, Ppid: 100, Cmd: "next-server"},                     // on :3800
			{Pid: 102, Ppid: 101, Cmd: "node", Slot: "4", Service: "web"}, // a build worker
			{Pid: 90, Ppid: 1, Cmd: "process-compose"},                    // another stack's, alive
			{Pid: 200, Ppid: 90, Cmd: "api", Slot: "4", Service: "api"},   // on :8880, looked after
			{Pid: 500, Ppid: 1, Cmd: "node", Slot: "5", Service: "web"},   // another worktree's
		},
		ports: map[int][]int{3800: {101}, 8880: {200}, 4000: {500}},
		dirs:  map[int]string{100: "/wt/a/apps/web", 200: "/wt/a", 500: "/wt/b/apps/web"},
	}
	m.install(t)
	if got := trees(leftovers(st, 4, all, roots)); got != "web:node:100+101+102" {
		t.Errorf("leftovers = %q", got)
	}

	// The same processes from another repository's slot 4.
	m.dirs[100] = "/elsewhere/apps/web"
	if got := trees(leftovers(st, 4, all, roots)); got != "" {
		t.Errorf("another repository's process was taken for a leftover: %q", got)
	}
	m.dirs[100] = "/wt/a-2/apps/web"
	if got := trees(leftovers(st, 4, all, roots)); got != "" {
		t.Errorf("a sibling directory's process was taken for a leftover: %q", got)
	}
	m.dirs[100] = "/wt/a/apps/web"

	// Started from someone's shell, with the stack's environment: it has a
	// parent, so it is theirs to stop.
	m.procs = append(m.procs, procInfo{Pid: 77, Ppid: 1, Cmd: "zsh"})
	m.procs[0].Ppid = 77
	if got := trees(leftovers(st, 4, all, roots)); got != "" {
		t.Errorf("a process with a parent was taken for a leftover: %q", got)
	}
	m.procs[0].Ppid = 1

	// The top process's own marks decide, not those of what is under it.
	m.procs[0].Slot, m.procs[0].Service = "", ""
	if got := trees(leftovers(st, 4, all, roots)); got != "" {
		t.Errorf("a tree hydra didn't start was taken for a leftover: %q", got)
	}
	m.procs[0].Slot, m.procs[0].Service = "4", "web"

	// Only what is asked for.
	if got := trees(leftovers(st, 4, []string{"api", "worker"}, roots)); got != "" {
		t.Errorf("leftovers for api and worker = %q", got)
	}
}

// What a step of the stack leaves running on purpose looks like a leftover
// in every way but one: it doesn't hold the port of the service whose marks
// it carries. A Docker VM started by a process of main's stack is the case
// that matters: stopping it takes every container down.
func TestWhatAStepLeavesRunningOnPurposeIsNotALeftover(t *testing.T) {
	st := leftoverSt(t)
	all := []string{"colima", "supabase", "api", "web", "worker"}
	m := &machine{
		procs: []procInfo{
			{Pid: 300, Ppid: 1, Cmd: "limactl", Slot: "0", Service: "colima"},    // the VM's host agent
			{Pid: 301, Ppid: 300, Cmd: "ssh", Slot: "0", Service: "colima"},      // forwards the containers' ports
			{Pid: 310, Ppid: 1, Cmd: "postgres", Slot: "0", Service: "supabase"}, // a daemon's own process
			{Pid: 320, Ppid: 1, Cmd: "worker", Slot: "0", Service: "worker"},     // no port to tell by
			{Pid: 330, Ppid: 1, Cmd: "Slack"},
		},
		// The VM forwards the database's port and, by accident, the api's.
		ports: map[int][]int{54321: {301}, 54322: {310}, 8080: {301}, 3000: {330}},
		dirs:  map[int]string{300: "/repo", 310: "/repo", 320: "/repo", 330: "/"},
	}
	m.install(t)
	if got := trees(leftovers(st, 0, all, []string{"/repo"})); got != "" {
		t.Errorf("leftovers = %q, want none", got)
	}
}

// When process-compose is gone, hydra's own process in the stack knows what
// it had started, port or no port, and stops what is still there: the
// services, not what a step left running on purpose, whose pid
// process-compose never had.
func TestWhatADeadProcessComposeLeftIsKnownByItsPids(t *testing.T) {
	taken := time.Date(2026, 11, 23, 20, 0, 0, 0, time.UTC) // when process-compose last said what it runs
	before, after := taken.Add(-time.Hour), taken.Add(time.Minute)
	m := &machine{
		procs: []procInfo{
			{Pid: 100, Ppid: 1, Cmd: "node", Slot: "4", Service: "web"},
			{Pid: 101, Ppid: 100, Cmd: "next-server"},
			{Pid: 320, Ppid: 1, Cmd: "worker", Slot: "4", Service: "worker"},
			{Pid: 330, Ppid: 1, Cmd: "bash"},                                  // a stand-in for main's copy: no marks
			{Pid: 300, Ppid: 1, Cmd: "limactl", Slot: "4", Service: "colima"}, // not one process-compose named
			{Pid: 400, Ppid: 1, Cmd: "renamed"},                               // its marks can't be read
			{Pid: 500, Ppid: 1, Cmd: "Slack"},                                 // the number went to another program since
			{Pid: 510, Ppid: 1, Cmd: "mystery"},                               // when it was started can't be read
			{Pid: 600, Ppid: 1, Cmd: "node", Slot: "5", Service: "web"},       // another stack's process
			{Pid: 610, Ppid: 1, Cmd: "node", Slot: "4", Service: "web"},       // another service's of this one
			{Pid: 700, Ppid: 650, Cmd: "api", Slot: "4", Service: "api"},      // someone looks after it
		},
		born: map[int]time.Time{100: before, 320: before, 330: before, 300: before, 400: before, 500: after, 600: before, 610: before, 700: before},
	}
	m.install(t)
	started := roster{at: taken, pids: map[string]int{
		"web": 100, "worker": 320, "temporal": 330, "odd": 400, "docs": 500, "jobs": 510, "reused": 600, "mobile": 610, "api": 700, "ended": 999,
	}}
	if got := trees(orphansOf(started, 4)); got != "odd:renamed:400 temporal:bash:330 web:node:100+101 worker:worker:320" {
		t.Errorf("orphansOf = %q", got)
	}
	// A roster nobody can date names nothing.
	if got := orphansOf(roster{pids: started.pids}, 4); got != nil {
		t.Errorf("orphansOf an undated roster = %v", got)
	}
	// With none of them left, which is the usual case, the processes aren't
	// even listed.
	m.listed = 0
	if got := orphansOf(roster{at: taken, pids: map[string]int{"ended": 999}}, 4); got != nil || m.listed != 0 {
		t.Errorf("with nothing left: %v, the processes listed %d times", got, m.listed)
	}
}

type fakePC struct {
	procs []pc.Process
	err   error
}

func (f fakePC) Processes() ([]pc.Process, error) { return f.procs, f.err }

func TestWhatProcessComposeRunsIsKeptAsItLastSaid(t *testing.T) {
	f := fakePC{procs: []pc.Process{
		{Name: "web", Pid: 100, Status: "Running"},
		{Name: "worker", Pid: 320, Status: "Running"},
		{Name: "temporal", Pid: 330, Status: "Running"}, // a stand-in for main's copy: a process too
		{Name: "supabase", Pid: 50, Status: "Launched"}, // a daemon: the pid is its launcher's
		{Name: "colima", Pid: 40, Status: "Completed"},  // ran and ended
		{Name: "api", Pid: 70, Status: "Restarting"},    // between two pids
		{Name: "mobile", Status: "Disabled"},
		{Name: "hydra-watch", Pid: 60, Status: "Running"}, // hydra's own look after themselves
		{Name: "hydra-router", Pid: 63, Status: "Running"},
		{Name: "hydra-ready-supabase", Pid: 61, Status: "Running"},
		{Name: "hydra-prepare-worker", Pid: 64, Status: "Running"},
		{Name: "hydra-ui", Pid: 62, Status: "Running"}, // the stack file's, whatever its name
	}}
	got := startedNow(f, roster{})
	if p := got.pids; len(p) != 4 || p["web"] != 100 || p["worker"] != 320 || p["temporal"] != 330 || p["hydra-ui"] != 62 {
		t.Errorf("startedNow = %v", got)
	}
	if time.Since(got.at) > time.Minute {
		t.Errorf("taken at %s", got.at)
	}
	// It stopped answering: what it said last is what it left, as of then.
	if again := startedNow(fakePC{err: fmt.Errorf("connection refused")}, got); len(again.pids) != 4 || !again.at.Equal(got.at) {
		t.Errorf("after it stopped answering: %v", again)
	}
}

// Nobody on any of the ports is the usual case, and costs one look at the
// listeners: the processes aren't listed at all.
func TestLookingForLeftoversIsCheapWhenThePortsAreFree(t *testing.T) {
	m := &machine{ports: map[int][]int{9999: {1}}}
	m.install(t)
	if got := leftovers(leftoverSt(t), 4, []string{"api", "web", "worker"}, []string{"/wt/a"}); got != nil {
		t.Errorf("leftovers = %v", got)
	}
	if m.listed != 0 {
		t.Errorf("the processes were listed %d times with every port free", m.listed)
	}
}

func TestProcessesAndTheirMarksAreReadFromPS(t *testing.T) {
	listing := strings.Join([]string{
		"  100     1 node /wt/a/apps/web/node_modules/.bin/../next/dist/bin/next dev HOME=/Users/me HYDRA_UPSTREAMS=A,B HYDRA_SERVICE=web HYDRA_SLOT=4 PORT=3800",
		"  101   100 next-server (v16.4.1)",
		"  200     1 ./bin/api TEMPORAL_NAMESPACE=default HYDRA_SLOT=0 HYDRA_SERVICE=api",
		"  300     1 /usr/sbin/cfprefsd agent",
		"  400   399 grep HYDRA_SLOT=",                       // names the mark, carries neither
		"  500     1 env NOT_HYDRA_SLOT=4 HYDRA_SERVICE=web", // one of the two
		"garbage",
		"",
	}, "\n")
	got := processesFromPS(listing)
	want := []procInfo{
		{Pid: 100, Ppid: 1, Cmd: "node", Slot: "4", Service: "web"},
		{Pid: 101, Ppid: 100, Cmd: "next-server"},
		{Pid: 200, Ppid: 1, Cmd: "api", Slot: "0", Service: "api"},
		{Pid: 300, Ppid: 1, Cmd: "cfprefsd"},
		{Pid: 400, Ppid: 399, Cmd: "grep"},
		{Pid: 500, Ppid: 1, Cmd: "env"},
	}
	if len(got) != len(want) {
		t.Fatalf("processesFromPS = %+v", got)
	}
	for _, w := range want {
		if got[w.Pid] != w {
			t.Errorf("process %d = %+v, want %+v", w.Pid, got[w.Pid], w)
		}
	}
}

func TestProcessesAndTheirMarksAreReadFromProc(t *testing.T) {
	environ := []byte("HOME=/home/me\x00HYDRA_SERVICE=web\x00NOT_HYDRA_SLOT=9\x00HYDRA_SLOT=4\x00PORT=3800\x00")
	got, ok := processFromProcFiles(101, environ, []byte("101 (next-server (v16) S 100 100 100 0 -1 4194304 1 0\n"))
	want := procInfo{Pid: 101, Ppid: 100, Cmd: "next-server (v16", Slot: "4", Service: "web"}
	if !ok || got != want {
		t.Errorf("processFromProcFiles = %+v, %v; want %+v", got, ok, want)
	}
	// One of the two marks, or an environment that can't be read: a process
	// all the same, and nobody's.
	for _, env := range []string{"HOME=/home/me\x00HYDRA_SLOT=4\x00", ""} {
		got, ok := processFromProcFiles(7, []byte(env), []byte("7 (sh) S 1 7 7 0\n"))
		if want := (procInfo{Pid: 7, Ppid: 1, Cmd: "sh"}); !ok || got != want {
			t.Errorf("with environ %q: %+v, %v; want %+v", env, got, ok, want)
		}
	}
	if _, ok := processFromProcFiles(7, environ, []byte("garbage")); ok {
		t.Error("a stat file that can't be read gave a process")
	}
}

// orphan starts a process that isn't this test's child, the way a leftover
// isn't hydra's, and returns its pid.
func orphan(t *testing.T, script string) int {
	t.Helper()
	out, err := exec.Command("sh", "-c", "("+script+") >/dev/null 2>&1 & echo $!").Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("no pid in %q", out)
	}
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

func TestALeftoverIsAskedToStopAndKilledIfItWont(t *testing.T) {
	old := leftoverGrace
	leftoverGrace = 300 * time.Millisecond
	t.Cleanup(func() { leftoverGrace = old })

	polite := orphan(t, "exec sleep 60")
	deaf := orphan(t, `trap "" TERM; exec sleep 60`)
	time.Sleep(100 * time.Millisecond) // let the second one set its trap
	gone := stopLeftovers([]leftover{{Service: "web", Pid: polite, Tree: []int{polite}}, {Service: "api", Pid: deaf, Tree: []int{deaf}}})
	if len(gone) != 2 {
		t.Errorf("stopped %d of 2: %+v", len(gone), gone)
	}
	for _, pid := range []int{polite, deaf} {
		if syscall.Kill(pid, 0) == nil {
			t.Errorf("pid %d is still there", pid)
		}
	}
	if stopLeftovers(nil) != nil {
		t.Error("nothing to stop, and something was")
	}
}

func TestTakenWording(t *testing.T) {
	st := leftoverSt(t)
	m := &machine{
		procs: []procInfo{
			{Pid: 100, Ppid: 1, Cmd: "node", Slot: "4", Service: "web"},
			{Pid: 101, Ppid: 100, Cmd: "next-server"},
			{Pid: 41, Ppid: 40, Cmd: "node"},
			{Pid: 40, Ppid: 1, Cmd: "zsh"},
		},
		ports: map[int][]int{3800: {101}},
		dirs:  map[int]string{100: "/wt/a/apps/web"},
	}
	m.install(t)
	restart := "hydra process restart web"

	// The service's own earlier copy: the restart stops it, parent and all.
	text, fix := takenBy(&holder{Port: 3800, Pid: 101, Cmd: "node", Dir: "/wt/a/apps/web"}, st, 4, "web", "/wt/a", restart)
	if !strings.Contains(text, "Port 3800 is held by node (pid 101), left over from an earlier run of this stack.") {
		t.Errorf("text for a leftover = %q", text)
	}
	if fix != "hydra process restart web   (stops the leftover first)" {
		t.Errorf("fix for a leftover = %q", fix)
	}

	// Started by hand in this checkout.
	text, fix = takenBy(&holder{Port: 3800, Pid: 41, Cmd: "node", Dir: "/wt/a/apps/web"}, st, 4, "web", "/wt/a", restart)
	if !strings.Contains(text, "runs in this checkout but not as part of its stack") || fix != "kill 41 && hydra process restart web" {
		t.Errorf("for a program started by hand here: %q, %q", text, fix)
	}

	// Someone else's program is never offered a kill.
	text, fix = takenBy(&holder{Port: 3800, Pid: 42, Cmd: "Slack", Dir: "/"}, st, 4, "web", "/wt/a", restart)
	if !strings.Contains(text, "held by another program: Slack (pid 42)") || strings.Contains(fix, "kill") {
		t.Errorf("for another program: %q, %q", text, fix)
	}
	// A worktree moves to another slot rather than stop it; main can't move.
	if !strings.HasPrefix(fix, "hydra slots free && hydra up -D") {
		t.Errorf("a worktree wasn't offered another slot: %q", fix)
	}
	if _, fix = takenBy(&holder{Port: 3000, Pid: 42, Cmd: "Slack", Dir: "/"}, st, 0, "web", "/main", restart); fix != "stop Slack, then: hydra process restart web" {
		t.Errorf("for another program on main's port: %q", fix)
	}
	// A sibling directory with the same prefix isn't this checkout.
	if _, fix = takenBy(&holder{Port: 3800, Pid: 43, Cmd: "node", Dir: "/wt/a-2/apps"}, st, 4, "web", "/wt/a", restart); strings.Contains(fix, "kill") {
		t.Errorf("a sibling worktree's program was offered a kill: %q", fix)
	}
}

func TestStoppedLeftoversAreNamed(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	old := leftoverGrace
	leftoverGrace = 300 * time.Millisecond
	t.Cleanup(func() { leftoverGrace = old })
	pid := orphan(t, "exec sleep 60")
	m := &machine{
		procs: []procInfo{{Pid: pid, Ppid: 1, Cmd: "node", Slot: "4", Service: "web"}},
		ports: map[int][]int{3800: {pid}},
		dirs:  map[int]string{pid: "/wt/a/apps/web"},
	}
	m.install(t)
	var said bytes.Buffer
	if !stopAndSay(leftBehind(leftoverSt(t), "/wt/a", 4, []string{"api", "web"}, []string{"/wt/a"}), &said) {
		t.Error("nothing was found")
	}
	want := "hydra: stopped what an earlier run of this stack left running: web (pid " + strconv.Itoa(pid) + ")\n"
	if said.String() != want {
		t.Errorf("said %q, want %q", said.String(), want)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Error("the leftover is still there")
	}
	if stopAndSay(nil, &said) {
		t.Error("nothing to stop, and it said there was")
	}
}

// What process-compose runs is written down beside the stack's socket, so
// that a start after both process-compose and the watcher were killed still
// knows what they left: also a worker, which holds no port to find it by.
func TestWhatProcessComposeRunsIsWrittenDownForTheNextStart(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	old := leftoverGrace
	leftoverGrace = 300 * time.Millisecond
	t.Cleanup(func() { leftoverGrace = old })
	st := leftoverSt(t)
	worker, web := orphan(t, "exec sleep 60"), orphan(t, "exec sleep 60")

	if remembered("/wt/a").pids != nil {
		t.Fatal("a note before anything was written")
	}
	running := fakePC{procs: []pc.Process{
		{Name: "worker", Pid: worker, Status: "Running"},
		{Name: "web", Pid: web, Status: "Running"},
		{Name: "hydra-watch", Pid: 60, Status: "Running"},
	}}
	started := track("/wt/a", running, roster{})
	got := remembered("/wt/a")
	if len(got.pids) != 2 || got.pids["worker"] != worker || got.pids["web"] != web || time.Since(got.at) > time.Minute {
		t.Fatalf("remembered = %v", got)
	}
	if remembered("/wt/b").pids != nil {
		t.Error("another worktree reads this one's note")
	}
	// Unchanged, it isn't written again; and a process-compose that stopped
	// answering leaves the note as it was.
	os.Remove(runningFile("/wt/a"))
	started = track("/wt/a", running, started)
	started = track("/wt/a", fakePC{err: fmt.Errorf("connection refused")}, started)
	if remembered("/wt/a").pids != nil {
		t.Error("the note was written again though nothing changed")
	}
	// One that never answered leaves no note at all.
	if never := track("/wt/c", fakePC{err: fmt.Errorf("connection refused")}, roster{}); never.pids != nil || remembered("/wt/c").pids != nil {
		t.Errorf("a note of a process-compose that never answered: %v", never)
	}
	remember("/wt/a", started.pids)
	noted := remembered("/wt/a").at

	// Both were killed. The worker is still the one the note names. The
	// number web had belongs to a program that was started since.
	m := &machine{
		procs: []procInfo{
			{Pid: worker, Ppid: 1, Cmd: "worker", Slot: "4", Service: "worker"},
			{Pid: web, Ppid: 1, Cmd: "node"},
		},
		born: map[int]time.Time{worker: noted.Add(-time.Minute), web: noted.Add(time.Minute)},
	}
	m.install(t)
	var said bytes.Buffer
	stopAndSay(leftBehind(st, "/wt/a", 4, []string{"api", "web", "worker"}, []string{"/wt/a"}), &said)
	want := "hydra: stopped what an earlier run of this stack left running: worker (pid " + strconv.Itoa(worker) + ")\n"
	if said.String() != want {
		t.Errorf("said %q, want %q", said.String(), want)
	}
	if syscall.Kill(worker, 0) == nil {
		t.Error("the worker is still there")
	}
	if syscall.Kill(web, 0) != nil {
		t.Error("a program that was started after the note was written was stopped")
	}
}

func TestWhenAProcessWasStartedIsRead(t *testing.T) {
	before := time.Now().Add(-2 * time.Second) // ps says it to the second
	pid := orphan(t, "exec sleep 60")
	born := processStart(pid)
	if born.Before(before) || born.After(time.Now().Add(time.Second)) {
		t.Errorf("processStart(%d) = %s, want about %s", pid, born, time.Now())
	}
	if gone := processStart(99999999); !gone.IsZero() {
		t.Errorf("a process that isn't there was started at %s", gone)
	}

	// Linux: field 22 of /proc/PID/stat, in hundredths of a second since boot.
	stat := []byte("4242 (next (dev) server) S 1 4242 4242 0 -1 4194304 1 0 0 0 0 0 0 0 20 0 1 0 123456 1000 10 18446744073709551615")
	system := []byte("cpu  1 2 3\nbtime 1795000000\nprocesses 5\n")
	if got, want := startFromProc(stat, system), time.Unix(1795000000, 0).Add(1234560*time.Millisecond); !got.Equal(want) {
		t.Errorf("startFromProc = %s, want %s", got, want)
	}
	if got := startFromProc([]byte("garbage"), system); !got.IsZero() {
		t.Errorf("startFromProc of garbage = %s", got)
	}
}
