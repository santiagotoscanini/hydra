package main

import (
	"strings"
	"testing"
)

// What holds a stack's port is flagged only when no process-compose is
// above it: the stack's own processes, and another stack's, are left alone,
// and so are daemons and container forwarders.
func TestFindStrays(t *testing.T) {
	gone := t.TempDir() + "/shop-old" // never made: a worktree that was removed
	here := t.TempDir()
	m := &machine{
		procs: []procInfo{
			{Pid: 100, Ppid: 1, Cmd: "process-compose"},
			{Pid: 101, Ppid: 100, Cmd: "bash"},
			{Pid: 102, Ppid: 101, Cmd: "node"},   // web, run by the stack
			{Pid: 200, Ppid: 1, Cmd: "temporal"}, // left by a removed worktree
			{Pid: 300, Ppid: 1, Cmd: "api"},      // started by hand here
			{Pid: 400, Ppid: 1, Cmd: "postgres"}, // supabase's, a daemon
			{Pid: 500, Ppid: 1, Cmd: "com.docker.backend"},
		},
		ports: map[int][]int{3000: {102}, 8080: {300}, 7233: {200}, 54321: {400}, 9000: {500}},
		dirs:  map[int]string{200: gone, 300: here},
	}
	m.install(t)
	want := map[int]portUse{
		3000:  {Root: here, Service: "web"},
		8080:  {Root: here, Service: "api"},
		7233:  {Root: here, Service: "temporal"},
		9000:  {Root: here, Service: "minio"},
		12345: {Root: here, Service: "idle"}, // nobody listens
	}
	got := findStrays(want)
	if len(got) != 2 {
		t.Fatalf("strays = %+v, want temporal and api", got)
	}
	tmp, api := got[0], got[1] // by port
	if tmp.Pid != 200 || !tmp.DirGone || tmp.Service != "temporal" {
		t.Errorf("temporal: %+v", tmp)
	}
	if api.Pid != 300 || api.DirGone || api.Dir != here {
		t.Errorf("api: %+v", api)
	}
	p := strayProblem(tmp)
	if p.Blocks || p.Fix != "kill 200" || !strings.Contains(p.Text, "port 7233 (temporal) is held by temporal (pid 200), which is not part of any stack") || !strings.Contains(p.Text, "which no longer exists (a worktree that was removed)") {
		t.Errorf("problem: %+v", p)
	}
}

// A daemon's ports and a Supabase stack's aren't looked at: they leave
// process-compose on purpose.
func TestStackPortsLeaveDaemonsOut(t *testing.T) {
	st := leftoverSt(t)
	want := map[int]portUse{}
	stackPorts(want, st, "/r", 0, []string{"supabase", "api", "web", "worker"})
	if len(want) != 2 || want[8080].Service != "api" || want[3000].Service != "web" {
		t.Errorf("ports = %v", want)
	}
}
