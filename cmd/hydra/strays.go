package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/santiagotoscanini/hydra/internal/config"
)

// A program that holds a port of a stack and is part of no stack is in the
// way: the service either fails to start on that port, or (worse) sees it
// taken, takes it for its own copy, and carries on with whatever answers
// there. A Temporal dev server left by a worktree that was removed held
// 7233 for days that way, and main used it without a word.
//
// hydra status names such a program: for each port of a service the
// checkout is meant to run (in main, every one that isn't off; in a
// worktree, what runs there, on its slot's ports), who listens on it, and
// whether that program descends from a process-compose. Whatever a stack
// runs does; what outlived its stack, or was started by hand, doesn't.
//
// Left out: services marked is_daemon and Supabase stacks, which detach
// from process-compose on purpose, and ports published by a container
// runtime, which a forwarder of the runtime holds.

// stray is a program holding a port of a checkout's stack that no stack runs.
type stray struct {
	Root    string // the checkout whose port it holds
	Service string
	Port    int
	Pid     int
	Cmd     string
	Dir     string // its working directory, "" when unknown
	DirGone bool   // that directory no longer exists
}

// portUse is a port a checkout's stack is meant to listen on.
type portUse struct {
	Root    string
	Service string
}

// forwarders are the programs that hold the ports a container runtime
// publishes: the port is a container's, not a stray's.
var forwarders = []string{"com.docker", "docker-proxy", "vpnkit", "OrbStack", "orbstack", "limactl", "gvproxy", "colima"}

// stackPorts adds to want the ports of the services st runs in slot, as
// listed in names.
func stackPorts(want map[int]portUse, st *config.Stack, root string, slot int, names []string) {
	for _, name := range names {
		s := st.Services[name]
		if s == nil || s.Kind == config.KindSupabase || isDaemon(st, name) {
			continue
		}
		for _, p := range st.SlotPorts(s, slot) {
			want[p] = portUse{Root: root, Service: name}
		}
	}
}

// mainPorts are the ports of what the main checkout runs: every service
// that isn't off.
func mainPorts(want map[int]portUse, ms *mainState) {
	var names []string
	for name := range ms.target.stack.Services {
		if !disabled(ms.target.stack, name) {
			names = append(names, name)
		}
	}
	stackPorts(want, ms.target.stack, ms.target.checkout.Root, 0, names)
}

// worktreePorts are the ports of what a worktree runs, on its slot's ports.
// One that never ran holds no slot, and so no ports.
func worktreePorts(want map[int]portUse, ws *wtState) {
	if ws.Slot == 0 {
		return
	}
	var names []string
	for _, row := range ws.Plan.Services {
		if row.Local && !row.Disabled {
			names = append(names, row.Source)
		}
	}
	stackPorts(want, ws.target.stack, ws.target.checkout.Root, ws.Slot, names)
}

// findStrays asks who listens on the ports in want: one lsof and one ps for
// all of them, and one more lsof per stray found, for its directory.
func findStrays(want map[int]portUse) []stray {
	if len(want) == 0 {
		return nil
	}
	held := listeners()
	var ports []int
	for p := range want {
		if len(held[p]) > 0 {
			ports = append(ports, p)
		}
	}
	if len(ports) == 0 {
		return nil
	}
	sort.Ints(ports)
	table := processTable()
	var out []stray
	seen := map[int]bool{}
	for _, port := range ports {
		for _, pid := range held[port] {
			p, ok := table[pid]
			if !ok || seen[pid] || underProcessCompose(table, pid) || isForwarder(p.Cmd) {
				continue
			}
			seen[pid] = true
			u := want[port]
			s := stray{Root: u.Root, Service: u.Service, Port: port, Pid: pid, Cmd: p.Cmd, Dir: workdir(pid)}
			if s.Dir != "" {
				_, err := os.Stat(s.Dir)
				s.DirGone = os.IsNotExist(err)
			}
			out = append(out, s)
		}
	}
	return out
}

// underProcessCompose reports whether a process descends from a
// process-compose: it is part of a stack, this one's or another's.
func underProcessCompose(table map[int]procInfo, pid int) bool {
	for seen := map[int]bool{}; pid > 1 && !seen[pid]; {
		seen[pid] = true
		p, ok := table[pid]
		if !ok {
			return false
		}
		if p.Cmd == "process-compose" {
			return true
		}
		pid = p.Ppid
	}
	return false
}

func isForwarder(cmd string) bool {
	for _, f := range forwarders {
		if strings.HasPrefix(cmd, f) {
			return true
		}
	}
	return false
}

// strayProblem words a stray for hydra status: a note, since the stack may
// run fine around it, with the kill that ends it.
func strayProblem(s stray) problem {
	text := fmt.Sprintf("port %d (%s) is held by %s (pid %d), which is not part of any stack", s.Port, s.Service, s.Cmd, s.Pid)
	switch {
	case s.DirGone:
		text += fmt.Sprintf(": it was started in %s, which no longer exists (a worktree that was removed)", s.Dir)
	case s.Dir != "":
		text += fmt.Sprintf(" (it runs in %s)", s.Dir)
	}
	text += ". The service fails on that port, or takes this program for its own copy."
	return problem{Text: text, Fix: fmt.Sprintf("kill %d", s.Pid)}
}

func strayProblems(list []stray) []problem {
	var out []problem
	for _, s := range list {
		out = append(out, strayProblem(s))
	}
	return out
}
