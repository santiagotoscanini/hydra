package main

import (
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/santiagotoscanini/hydra/internal/journal"
	"github.com/santiagotoscanini/hydra/internal/pc"
)

// sampleEvery is how often a stack's usage goes into the journal.
const sampleEvery = time.Minute

// monitor writes what a stack does into the journal. It rides on the look
// hydra's own process in the stack already takes every two seconds (the
// watcher's in a worktree, the router's in main), so it costs no extra call
// to process-compose: a change in a process's state is written as it is
// seen, and once a minute what every process holds.
type monitor struct {
	client  *pc.Client
	machine bool // also sample the machine (main's does, once for everyone)
	was     map[string]pc.Process
	sampled time.Time
}

func newMonitor(client *pc.Client, machine bool) *monitor {
	return &monitor{client: client, machine: machine}
}

// Processes is the client's, observed on the way.
func (m *monitor) Processes() ([]pc.Process, error) {
	procs, err := m.client.Processes()
	if err != nil {
		return procs, err
	}
	m.observe(procs, time.Now())
	return procs, nil
}

func (m *monitor) observe(procs []pc.Process, now time.Time) {
	first := m.was == nil
	now_ := map[string]pc.Process{}
	for _, p := range procs {
		now_[p.Name] = p
		before, seen := m.was[p.Name]
		if !first && seen && !changed(before, p) {
			continue
		}
		f := journal.Fields{"name": p.Name, "status": p.Status, "health": healthName(p.Health()), "restarts": p.Restarts, "exit": p.ExitCode, "pid": p.Pid}
		if p.HasReadyProbe {
			f["ready"] = p.IsReady
		}
		if seen {
			f["was"] = before.Status
			if before.Health() != p.Health() {
				f["was_health"] = healthName(before.Health())
			}
		}
		if first {
			f["first_look"] = true
		}
		// The reason, while process-compose still holds it.
		if !first && (p.Health() == pc.Failed || p.Restarts > before.Restarts || p.StoppedAnswering()) {
			if lines, err := m.client.Logs(p.Name, 30); err == nil {
				f["output"] = lines
			}
		}
		journal.Log("process", f)
	}
	for name := range m.was {
		if _, still := now_[name]; !still {
			journal.Log("process", journal.Fields{"name": name, "status": "removed from the project"})
		}
	}
	m.was = now_
	if now.Sub(m.sampled) >= sampleEvery {
		m.sampled = now
		m.sample(procs)
	}
}

func changed(a, b pc.Process) bool {
	return a.Status != b.Status || a.IsReady != b.IsReady || a.Restarts != b.Restarts || a.ExitCode != b.ExitCode || a.Pid != b.Pid
}

// sample writes what each running process and its children hold: resident
// memory in MB and CPU in percent of one core, as ps counts them. Resident
// memory counts files a process maps (Turbopack's cache), so it reads high
// for next dev; the trend is what to look at.
func (m *monitor) sample(procs []pc.Process) {
	tree := psTree()
	if tree == nil {
		return
	}
	per := map[string]any{}
	var mb, cpu float64
	for _, p := range procs {
		if p.Pid <= 0 || !p.IsRunning {
			continue
		}
		u := tree.sum(p.Pid)
		if u.n == 0 {
			continue
		}
		per[p.Name] = map[string]any{"mb": round1(u.mb), "cpu": round1(u.cpu), "procs": u.n}
		mb += u.mb
		cpu += u.cpu
	}
	f := journal.Fields{"mb": round1(mb), "cpu": round1(cpu), "procs": per}
	if m.machine {
		for k, v := range machineUsage() {
			f[k] = v
		}
	}
	journal.Log("usage", f)
}

type psUsage struct {
	mb, cpu float64
	n       int
}

type psTable struct {
	kids map[int][]int
	own  map[int]psUsage
}

func psTree() *psTable {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,rss=,pcpu=").Output()
	if err != nil {
		return nil
	}
	t := &psTable{kids: map[int][]int{}, own: map[int]psUsage{}}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 4 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		kb, _ := strconv.ParseFloat(f[2], 64)
		cpu, _ := strconv.ParseFloat(strings.Replace(f[3], ",", ".", 1), 64)
		t.kids[ppid] = append(t.kids[ppid], pid)
		t.own[pid] = psUsage{mb: kb / 1024, cpu: cpu, n: 1}
	}
	return t
}

func (t *psTable) sum(pid int) psUsage {
	var total psUsage
	seen := map[int]bool{}
	for todo := []int{pid}; len(todo) > 0; {
		p := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if seen[p] {
			continue
		}
		seen[p] = true
		if u, ok := t.own[p]; ok {
			total.mb += u.mb
			total.cpu += u.cpu
			total.n++
		}
		todo = append(todo, t.kids[p]...)
	}
	return total
}

// machineUsage is the machine's memory as macOS sees it: the free
// percentage memory_pressure reports, and swap in use. Elsewhere, nothing.
func machineUsage() map[string]any {
	out := map[string]any{}
	if b, err := exec.Command("memory_pressure", "-Q").Output(); err == nil {
		if _, pct, ok := strings.Cut(string(b), "percentage:"); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(pct), "%"))); err == nil {
				out["machine_free_pct"] = n
			}
		}
	}
	if b, err := exec.Command("sysctl", "-n", "vm.swapusage").Output(); err == nil {
		// total = 10240.00M  used = 8660.19M  free = 1579.81M  (encrypted)
		f := strings.Fields(string(b))
		for i := 0; i+2 < len(f); i++ {
			if f[i] == "used" {
				if n, err := strconv.ParseFloat(strings.TrimSuffix(f[i+2], "M"), 64); err == nil {
					out["swap_used_mb"] = round1(n)
				}
			}
		}
	}
	return out
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }
