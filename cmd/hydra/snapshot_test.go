package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/santiagotoscanini/hydra/internal/pc"
	"github.com/santiagotoscanini/hydra/internal/plan"
	"github.com/santiagotoscanini/hydra/internal/stack"
)

func TestWhatTheSnapshotSaysAboutAWorktree(t *testing.T) {
	st := checkSt(t)
	mainT := &target{stack: st}
	mainT.checkout.Root, mainT.checkout.Main = "/repo/main", "/repo/main"
	ms := &mainState{target: mainT, Head: "abc", Branch: "main", Up: true, Client: pc.TCP("127.0.0.1", 1), Record: &baselineRecord{Commit: "abc"},
		Procs: map[string]pc.Process{
			"db": {Status: "Launched", IsReady: "Ready"}, "migrate": {Status: "Completed"}, "deps": {Status: "Completed"},
			"api": {Status: "Running"}, "web": {Status: "Running"}, "mobile": {Status: "Disabled"},
		}}
	wtT := &target{stack: st}
	wtT.checkout.Root, wtT.checkout.Main = "/repo/wt", "/repo/main"
	build := func(slot int, up bool) snapWorktree {
		t.Helper()
		p, err := plan.Build(st, plan.Request{Slot: 1, Changed: []string{"api/users.go"}, MainChanged: []string{"web/page.tsx"}})
		if err != nil {
			t.Fatal(err)
		}
		ws := &wtState{target: wtT, Name: "cart", Slot: slot, Plan: p, Client: pc.TCP("127.0.0.1", 1), Up: up}
		if up {
			ws.Procs = map[string]pc.Process{
				"api": {Status: "Running", IsRunning: true, Pid: 42}, "deps": {Status: "Completed"},
				"hydra-watch": {Status: "Running", IsRunning: true},
			}
		}
		mem := func(pid int) int64 { return int64(pid) * 1000 }
		return snapWorktreeOf(ms, stack.Worktree{Root: "/repo/wt", Branch: "cart"}, &wtPlan{ws: ws, at: time.Now()}, &wtGit{commit: "def", behind: 3, ahead: 1, at: time.Now()}, mem)
	}
	byName := func(w snapWorktree) map[string]snapService {
		m := map[string]snapService{}
		for _, s := range w.Services {
			m[s.Name] = s
		}
		return m
	}

	// Its stack is up: the api runs here, web is main's copy at its port.
	w := build(1, true)
	svc := byName(w)
	// Links are for a browser, which gets localhost: a dev server treats a
	// page opened by IP address as another origin.
	if a := svc["api"]; a.Runs != "here" || a.Port != 8180 || a.URL != "http://localhost:8180" || a.Health != "up" || a.Memory != 42000 {
		t.Errorf("api = %+v", a)
	}
	if web := svc["web"]; web.Runs != "routed" || web.Port != 3100 || web.Health != "up" {
		t.Errorf("web = %+v", web)
	}
	if db := svc["db"]; db.Runs != "main" || !db.Used || db.State != "ready" {
		t.Errorf("db = %+v", db)
	}
	if m := svc["mobile"]; m.Runs != "main" || m.Used {
		t.Errorf("mobile = %+v: nothing here uses it", m)
	}
	if w.Memory != 42000 || w.Behind != 3 || w.Branch != "cart" {
		t.Errorf("worktree = %+v", w)
	}
	// main changed web since the branch left, and web serves this worktree.
	if len(w.OutOfStep) != 1 || w.OutOfStep[0].Service != "web" {
		t.Errorf("out of step = %+v", w.OutOfStep)
	}
	if len(w.Problems) == 0 || !w.Problems[0].Blocks {
		t.Errorf("problems = %+v", w.Problems)
	}

	// It never ran: no slot, so no ports of its own to show.
	w = build(0, false)
	svc = byName(w)
	if a := svc["api"]; a.Port != 0 || a.URL != "" || a.State != "not started" {
		t.Errorf("api before a slot = %+v", a)
	}
	if web := svc["web"]; web.Port != 0 {
		t.Errorf("web before a slot = %+v", web)
	}
	if db := svc["db"]; db.Port != 5432 {
		t.Errorf("main's db keeps its port: %+v", db)
	}

	// The whole thing is what the dashboard reads.
	data, err := json.Marshal(snapshot{Version: snapshotVersion, Repo: "/repo/main", Main: snapMainOf(ms, func(int) int64 { return 0 }), Worktrees: []snapWorktree{w}})
	if err != nil || !strings.Contains(string(data), `"outOfStep":[{"service":"web"`) {
		t.Errorf("json: %v %s", err, data)
	}
}

func TestMemoryCountsAProcessAndItsChildren(t *testing.T) {
	if got := processTrees()(os.Getpid()); got <= 0 {
		t.Errorf("this process holds %d bytes", got)
	}
	if got := processTrees()(0); got != 0 {
		t.Errorf("pid 0 = %d", got)
	}
}

func TestAFixIsRunWhenItIsAHydraCommand(t *testing.T) {
	for fix, want := range map[string]string{
		"hydra update": "hydra update",
		"hydra update   (or, for this worktree alone: git merge main)":                               "hydra update",
		"cd '/repo/main' && hydra up -D":                                                             "cd '/repo/main' && hydra up -D",
		"hydra up web   (runs a copy here instead)":                                                  "hydra up web",
		"if a service uses them, add them to its paths (`hydra up SERVICE` runs one here meanwhile)": "",
		"psql postgresql://x -c 'ALTER' && hydra process restart supabase   (in the main checkout)":  "",
		"": "",
	} {
		if got := runnableFix(fix); got != want {
			t.Errorf("runnableFix(%q) = %q, want %q", fix, got, want)
		}
	}
}
