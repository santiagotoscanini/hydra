package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/pc"
	"github.com/santiagotoscanini/hydra/internal/plan"
)

const checkStack = `
x-hydra: { slot_stride: 100, max_slots: 5 }
processes:
  deps:
    command: install
    x-hydra: { follow: true, paths: ["lock.txt"] }
  db:
    command: db
    is_daemon: true
    x-hydra: { shared: true, port: 5432, paths: ["db/**"] }
  migrate:
    command: migrate
    x-hydra: { shared: true, paths: ["db/migrations/**"] }
  api:
    command: api
    depends_on:
      deps: { condition: process_completed_successfully }
      migrate: { condition: process_completed_successfully }
    x-hydra: { port: 8080, paths: ["api/**", "!**/*_test.go"], upstreams: { DB: db } }
  web:
    command: web
    depends_on:
      api: { condition: process_started }
    x-hydra: { port: 3000, paths: ["web/**"], routed: true, upstreams: { API_URL: api } }
  mobile:
    command: expo
    disabled: true
    x-hydra: { paths: ["mobile/**"] }
`

func checkSt(t *testing.T) *config.Stack {
	t.Helper()
	st, err := config.Parse([]byte(checkStack))
	if err != nil {
		t.Fatal(err)
	}
	st.Hydra.BaseRef = "main"
	return st
}

func names(list []stale) string {
	var out []string
	for _, s := range list {
		out = append(out, s.Name)
	}
	return strings.Join(out, ",")
}

func TestAffectedIsWhatAnUpdateRestarts(t *testing.T) {
	st := checkSt(t)
	procs := map[string]pc.Process{
		"deps":    {Name: "deps", Status: "Completed"},
		"db":      {Name: "db", Status: "Launched", IsReady: "Ready"},
		"migrate": {Name: "migrate", Status: "Completed"},
		"api":     {Name: "api", Status: "Running"},
		"web":     {Name: "web", Status: "Running"},
		"mobile":  {Name: "mobile", Status: "Disabled"},
	}
	files := []string{"api/users.go", "api/users_test.go", "db/migrations/2.sql", "mobile/app.tsx", "docs/x.md"}
	// Not db: it holds data. Not mobile: nobody started it. Not web or deps:
	// none of their files changed.
	if got := names(affected(st, procs, sourceNames(st), files)); got != "api,migrate" {
		t.Errorf("affected = %q, want api,migrate", got)
	}
	// A test file alone restarts nothing.
	if got := names(affected(st, procs, sourceNames(st), []string{"api/users_test.go"})); got != "" {
		t.Errorf("affected by a test file = %q", got)
	}
	// mobile is off by default. Someone ran it and it exited, or crashed: an
	// update leaves it off. Only while it runs does an update restart it.
	for _, state := range []pc.Process{{Status: "Completed"}, {Status: "Completed", ExitCode: 1}} {
		procs["mobile"] = state
		if got := names(affected(st, procs, sourceNames(st), []string{"mobile/app.tsx"})); got != "" {
			t.Errorf("mobile %+v: affected = %q, want nothing", state, got)
		}
	}
	procs["mobile"] = pc.Process{Status: "Running", IsRunning: true}
	if got := names(affected(st, procs, sourceNames(st), []string{"mobile/app.tsx"})); got != "mobile" {
		t.Errorf("running mobile: affected = %q", got)
	}
}

func TestJobsAreToldFromServices(t *testing.T) {
	st := checkSt(t)
	// migrate had failed: still a job, because api waits for it to complete.
	if !isJob(st, "migrate", pc.Process{Status: "Completed", ExitCode: 1}) {
		t.Error("migrate should be a job")
	}
	if isJob(st, "api", pc.Process{Status: "Running"}) || isJob(st, "web", pc.Process{Status: "Completed", ExitCode: 1}) {
		t.Error("api and web are services")
	}
}

func TestWhatAWorktreeTakesFromMain(t *testing.T) {
	st := checkSt(t)
	// The branch changed the api.
	p, err := plan.Build(st, plan.Request{Slot: 1, Changed: []string{"api/users.go"}})
	if err != nil {
		t.Fatal(err)
	}
	got := uses(st, p)
	want := map[string]string{
		"db":      "api here calls it",
		"migrate": "api here waits for it",
		"web":     "this worktree's port for it is :3100",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("uses = %v\nwant %v", got, want)
	}
}

func TestWorktreeProblemsBeforeItStarts(t *testing.T) {
	st := checkSt(t)
	tgt := &target{stack: st}
	tgt.checkout.Root, tgt.checkout.Main = "/repo/wt", "/repo/main"
	mainT := &target{stack: st}
	mainT.checkout.Root, mainT.checkout.Main = "/repo/main", "/repo/main"
	build := func(req plan.Request) *wtState {
		t.Helper()
		req.Slot = 1
		p, err := plan.Build(st, req)
		if err != nil {
			t.Fatal(err)
		}
		return &wtState{target: tgt, Plan: p}
	}
	up := func() *mainState {
		return &mainState{target: mainT, Head: "abc", Branch: "main", Up: true, Client: pc.TCP("127.0.0.1", 1), Procs: map[string]pc.Process{
			"db": {Status: "Launched", IsReady: "Ready"}, "migrate": {Status: "Completed"},
			"api": {Status: "Running"}, "web": {Status: "Running"}, "deps": {Status: "Completed"},
		}, Record: &baselineRecord{Commit: "abc"}}
	}
	texts := func(ps []problem) string {
		var out []string
		for _, p := range ps {
			mark := "warn: "
			if p.Blocks {
				mark = "BLOCK: "
			}
			out = append(out, mark+p.Text)
		}
		return strings.Join(out, "\n")
	}

	// All well.
	if ps := worktreeProblems(up(), build(plan.Request{Changed: []string{"api/users.go"}}), false); len(ps) != 0 {
		t.Errorf("want no problems, got:\n%s", texts(ps))
	}
	// Main is down.
	down := up()
	down.Up, down.Procs = false, nil
	ps := worktreeProblems(down, build(plan.Request{Changed: []string{"api/users.go"}}), false)
	if got := texts(ps); !strings.Contains(got, "BLOCK: main's stack isn't running") || !strings.Contains(got, "takes from it: db, migrate, web") {
		t.Errorf("main down:\n%s", got)
	}
	// Main's stack is stale in something this worktree doesn't use: a note,
	// not a refusal. In something it uses: a refusal.
	st2 := up()
	st2.Stale = []stale{{Name: "api", Files: []string{"api/x.go"}}}
	st2.Record = &baselineRecord{Commit: "old"}
	if got := texts(worktreeProblems(st2, build(plan.Request{Changed: []string{"api/users.go"}}), false)); !strings.Contains(got, "warn: main's files moved") {
		t.Errorf("stale api, own api:\n%s", got)
	}
	if got := texts(worktreeProblems(st2, build(plan.Request{Changed: []string{"web/page.tsx"}}), false)); !strings.Contains(got, "BLOCK: main's files moved") {
		t.Errorf("stale api, main's api used:\n%s", got)
	}
	// A service it takes from main has failed there.
	broken := up()
	broken.Procs["migrate"] = pc.Process{Name: "migrate", Status: "Completed", ExitCode: 1}
	if got := texts(worktreeProblems(broken, build(plan.Request{Changed: []string{"api/users.go"}}), false)); !strings.Contains(got, "BLOCK: main's migrate exited with code 1 (api here waits for it)") {
		t.Errorf("failed migrate:\n%s", got)
	}
	// The branch is behind main in what it uses; and the other way round.
	behind := build(plan.Request{Changed: []string{"web/page.tsx"}, MainChanged: []string{"api/new.go"}})
	if got := texts(worktreeProblems(up(), behind, false)); !strings.Contains(got, "BLOCK: this branch is behind main") || !strings.Contains(got, "api (web here uses it): api/new.go") {
		t.Errorf("behind:\n%s", got)
	}
	ahead := build(plan.Request{Changed: []string{"web/page.tsx"}, MainChanged: []string{"api/new.go"}, MainBehind: true})
	if got := texts(worktreeProblems(up(), ahead, false)); !strings.Contains(got, "BLOCK: main is older than the commit this branch started from") {
		t.Errorf("main behind:\n%s", got)
	}
	// A changed file nobody claims is a note.
	if got := texts(worktreeProblems(up(), build(plan.Request{Changed: []string{"packages/new/x.ts"}}), false)); !strings.Contains(got, "warn: changed, but in no service's paths") {
		t.Errorf("unclaimed:\n%s", got)
	}
}

func TestProcessComposeAddress(t *testing.T) {
	t.Setenv("PC_ADDRESS", "")
	t.Setenv("PC_PORT_NUM", "")
	if h, p := pcAddress(nil); h != "localhost" || p != 8080 {
		t.Errorf("defaults = %s:%d", h, p)
	}
	t.Setenv("PC_PORT_NUM", "8099")
	t.Setenv("PC_ADDRESS", "127.0.0.1")
	if h, p := pcAddress([]string{"-D"}); h != "127.0.0.1" || p != 8099 {
		t.Errorf("env = %s:%d", h, p)
	}
	if h, p := pcAddress([]string{"-D", "-p", "8198", "--address=0.0.0.0"}); h != "0.0.0.0" || p != 8198 {
		t.Errorf("flags = %s:%d", h, p)
	}
}

func TestShSplitUndoesShQuote(t *testing.T) {
	words := []string{"exec", "/opt/bin/hydra", "watch", "--root", "/tmp/my repo/it's here", "--with", "web"}
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = shQuote(w)
	}
	if got := shSplit(strings.Join(quoted, " ")); !reflect.DeepEqual(got, words) {
		t.Errorf("shSplit = %q\nwant %q", got, words)
	}
}

func TestAStackDoesNotInheritAnAgentsMarks(t *testing.T) {
	got := withoutAgentMarks([]string{"PATH=/bin", "AI_AGENT=claude", "CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=sdk", "CLAUDE_CONFIG_DIR=/x", "CURSOR_AGENT=1", "HOME=/h"})
	if want := []string{"PATH=/bin", "CLAUDE_CONFIG_DIR=/x", "HOME=/h"}; !reflect.DeepEqual(got, want) {
		t.Errorf("env = %q, want %q", got, want)
	}
}
