package render

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/plan"
)

const stack = `
processes:
  db:
    command: db
    readiness_probe: { http_get: { host: 127.0.0.1, port: 5432, path: / } }
    shutdown: { command: db stop }
    is_daemon: true
    x-hydra: { shared: true }
  migrate:
    command: migrate
    x-hydra: { shared: true }
  deps:
    command: install
    x-hydra: { follow: true }
  api:
    command: api
    log_location: logs/api.log
    depends_on:
      db: { condition: process_healthy }
      migrate: { condition: process_completed_successfully }
      deps: { condition: process_completed_successfully }
    readiness_probe: { http_get: { host: 127.0.0.1, port: 8080, path: /health } }
    watch: { paths: [ { path: ./api } ] }
    x-hydra:
      port: 8080
      paths: ["api/**"]
      url: "http://{host}:{port}"
      prepare: "setup {value}"
`

func TestRenderWorktree(t *testing.T) {
	st, err := config.Parse([]byte(stack))
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(st, plan.Request{Slot: 2, Changed: []string{"api/main.go"}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := Render(st, p, Options{Dir: "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Processes map[string]map[string]any `yaml:"processes"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	api := doc.Processes["api"]
	if port := api["readiness_probe"].(map[string]any)["http_get"].(map[string]any)["port"]; port != 10080 {
		t.Errorf("api probe port = %v, want 10080", port)
	}
	if api["log_location"] != "/repo/logs/api.log" {
		t.Errorf("log_location = %v", api["log_location"])
	}
	if !strings.Contains(string(data), "path: /repo/api") {
		t.Errorf("watch path not made absolute:\n%s", data)
	}
	db := doc.Processes["db"]
	if db["namespace"] != "main" || db["shutdown"] != nil || db["is_daemon"] != nil {
		t.Errorf("db stand-in must only wait: %v", db)
	}
	if db["readiness_probe"].(map[string]any)["http_get"].(map[string]any)["port"] != 5432 {
		t.Errorf("db stand-in probe should watch the baseline port: %v", db)
	}
	if doc.Processes["migrate"]["command"] != "true" {
		t.Errorf("migrate stand-in should complete at once: %v", doc.Processes["migrate"])
	}
	if doc.Processes["deps"]["namespace"] == "main" {
		t.Errorf("deps follows api, it should run for real")
	}
	if doc.Processes["hydra-prepare-api"]["command"] != "setup http://127.0.0.1:10080" {
		t.Errorf("prepare = %v", doc.Processes["hydra-prepare-api"])
	}
	if _, ok := api["depends_on"].(map[string]any)["hydra-prepare-api"]; !ok {
		t.Errorf("api must wait for its prepare step")
	}
}

const routedStack = `
x-hydra: { slot_stride: 100, max_slots: 5 }
processes:
  api:
    command: api
    x-hydra: { port: 8080, paths: ["api/**"] }
  web:
    command: web
    environment: ["NODE_OPTIONS=--max-old-space-size=4096"]
    depends_on:
      api: { condition: process_started }
    x-hydra:
      port: 3000
      paths: ["web/**"]
      routed: node
      upstreams: { API_URL: api }
`

func TestBaselineOfARoutedService(t *testing.T) {
	st, err := config.Parse([]byte(routedStack))
	if err != nil {
		t.Fatal(err)
	}
	data, err := Render(st, plan.BaselinePlan(st), Options{Dir: "/repo", Router: "exec hydra router", NodeHook: "/repo/.git/hydra hook.cjs"})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Processes map[string]struct {
			Command     string         `yaml:"command"`
			Environment []string       `yaml:"environment"`
			DependsOn   map[string]any `yaml:"depends_on"`
		} `yaml:"processes"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Processes["hydra-router"].Command != "exec hydra router" {
		t.Errorf("no router process: %+v", doc.Processes["hydra-router"])
	}
	web := doc.Processes["web"]
	env := strings.Join(web.Environment, "\n")
	for _, want := range []string{
		"API_URL=http://127.0.0.1:8680", // the router's port for the api
		"HYDRA_ROUTED_ORIGINS=http://127.0.0.1:8680",
		`NODE_OPTIONS=--max-old-space-size=4096 --require "/repo/.git/hydra hook.cjs"`,
	} {
		if !strings.Contains(env, want) {
			t.Errorf("web env lacks %q:\n%s", want, env)
		}
	}
	if web.DependsOn["hydra-router"] == nil || web.DependsOn["api"] == nil {
		t.Errorf("web depends_on = %v", web.DependsOn)
	}
	// The api itself is untouched.
	if e := strings.Join(doc.Processes["api"].Environment, " "); strings.Contains(e, "NODE_OPTIONS") || strings.Contains(e, "ROUTED") {
		t.Errorf("api env = %q", e)
	}

	// A worktree's file has neither the router nor the hook.
	p, err := plan.Build(st, plan.Request{Slot: 1, Changed: []string{"web/page.tsx"}})
	if err != nil {
		t.Fatal(err)
	}
	data, err = Render(st, p, Options{Dir: "/repo", Router: "exec hydra router", NodeHook: "/hook.cjs"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "hydra-router") || strings.Contains(string(data), "hook.cjs") {
		t.Errorf("worktree file mentions the router or the hook:\n%s", data)
	}
}

func TestWorktreeSupabaseIsAStackOfItsOwn(t *testing.T) {
	st, err := config.Parse([]byte(`
x-hydra: { slot_stride: 200 }
processes:
  supabase:
    command: supabase start
    is_daemon: true
    readiness_probe: { http_get: { host: 127.0.0.1, port: 54321, path: /rest/v1/ } }
    shutdown: { command: supabase stop, timeout_seconds: 60 }
    x-hydra: { kind: supabase, paths: ["supabase/**"] }
`))
	if err != nil {
		t.Fatal(err)
	}
	st.Hydra.Exe = "/bin/hydra"
	p, err := plan.Build(st, plan.Request{Slot: 1, Changed: []string{"supabase/migrations/1.sql"}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(st, p, Options{Dir: "/wt"})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Processes map[string]struct {
			Command        string         `yaml:"command"`
			Environment    []string       `yaml:"environment"`
			Shutdown       map[string]any `yaml:"shutdown"`
			ReadinessProbe struct {
				HTTPGet struct {
					Port int `yaml:"port"`
				} `yaml:"http_get"`
			} `yaml:"readiness_probe"`
		} `yaml:"processes"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	sb := doc.Processes["supabase"]
	// hydra's steps go around the stack file's command: one before, and one
	// after that runs whether or not the start worked.
	if want := "mkdir -p '/wt/supabase/.temp/tmp' && '/bin/hydra' prepare supabase --before || exit 1\n(\nsupabase start\n)\n'/bin/hydra' prepare supabase --started $?"; sb.Command != want {
		t.Errorf("command = %q, want %q", sb.Command, want)
	}
	env := strings.Join(sb.Environment, " ")
	for _, want := range []string{"SUPABASE_EXPERIMENTAL_STACK=1", "SUPABASE_API_PORT=54521", "SUPABASE_DB_PORT=54522", "TMPDIR=/wt/supabase/.temp/tmp"} {
		if !strings.Contains(env, want) {
			t.Errorf("environment lacks %s: %s", want, env)
		}
	}
	// The stop names the stack itself: run without that environment, it
	// would stop the baseline's Supabase. The stack file's stop goes by the
	// directory, so it only runs while that is still a worktree; hydra's
	// step follows it, for a stack the CLI's own stop no longer finds.
	stop, _ := sb.Shutdown["command"].(string)
	if !strings.HasPrefix(stop, "export SUPABASE_EXPERIMENTAL_STACK='1' ") || !strings.Contains(stop, "SUPABASE_API_PORT='54521'") ||
		!strings.HasSuffix(stop, "\n'/bin/hydra' prepare supabase --stopping || exit 0\nsupabase stop\n'/bin/hydra' prepare supabase --stopped") {
		t.Errorf("shutdown command = %q", stop)
	}
	if sb.Shutdown["timeout_seconds"] != 60 {
		t.Errorf("shutdown lost its timeout: %v", sb.Shutdown)
	}
	if sb.ReadinessProbe.HTTPGet.Port != 54521 {
		t.Errorf("probe port = %d", sb.ReadinessProbe.HTTPGet.Port)
	}

	// The baseline runs the command as written.
	base, err := Render(st, plan.BaselinePlan(st), Options{Dir: "/main"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(base), "SUPABASE_EXPERIMENTAL_STACK") || strings.Contains(string(base), "prepare supabase") {
		t.Errorf("baseline render touched supabase:\n%s", base)
	}
}

func TestBaselineIsStatic(t *testing.T) {
	st, err := config.Parse([]byte(strings.Replace(stack, `prepare: "setup {value}"`,
		`baseline_command: "build && exec ./api"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Processes map[string]map[string]any `yaml:"processes"`
	}
	render := func(p *plan.Plan) map[string]any {
		t.Helper()
		data, err := Render(st, p, Options{Dir: "/repo"})
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Processes["api"]
	}
	// Main: the lean command, and no file watcher; `hydra update` restarts it.
	api := render(plan.BaselinePlan(st))
	if api["command"] != "build && exec ./api" || api["watch"] != nil {
		t.Errorf("baseline api = %v, watch = %v", api["command"], api["watch"])
	}
	// A worktree keeps the dev command and its reload.
	p, err := plan.Build(st, plan.Request{Slot: 1, Changed: []string{"api/main.go"}})
	if err != nil {
		t.Fatal(err)
	}
	api = render(p)
	if api["command"] != "api" || api["watch"] == nil {
		t.Errorf("worktree api = %v, watch = %v", api["command"], api["watch"])
	}
}

func TestAProcessAskedForByNameIsSwitchedOn(t *testing.T) {
	st, err := config.Parse([]byte(`
processes:
  mobile:
    command: expo
    disabled: true
    x-hydra: { port: 8084, paths: ["mobile/**"] }
`))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Processes map[string]map[string]any `yaml:"processes"`
	}
	render := func(req plan.Request) map[string]any {
		t.Helper()
		req.Slot = 1
		p, err := plan.Build(st, req)
		if err != nil {
			t.Fatal(err)
		}
		data, err := Render(st, p, Options{Dir: "/repo"})
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Processes["mobile"]
	}
	// Changed, but nobody asked: listed, and off until started.
	if m := render(plan.Request{Changed: []string{"mobile/app.tsx"}}); m["disabled"] != true {
		t.Errorf("changed mobile should stay off: %v", m)
	}
	// `hydra up mobile`: on.
	if m := render(plan.Request{With: []string{"mobile"}}); m == nil || m["disabled"] != nil {
		t.Errorf("requested mobile should be on: %v", m)
	}
	// The main checkout: off, as written. Nothing there asks for it by name.
	data, err := Render(st, plan.BaselinePlan(st), Options{Dir: "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if m := doc.Processes["mobile"]; m["disabled"] != true {
		t.Errorf("the baseline must keep a disabled process off: %v", m)
	}
}

func TestBaselineWithATLSFront(t *testing.T) {
	st, err := config.Parse([]byte(`
processes:
  db:
    command: db
    x-hydra: { shared: true, port: 5432, tls: 5443 }
  web:
    command: web
    environment: ["FOO=1"]
    x-hydra:
      port: 3000
      upstreams:
        DB_URL: { service: db, baseline_value: "https://{host}:{tls}" }
  job:
    command: job
    environment: ["NODE_EXTRA_CA_CERTS=/my/own.pem"]
    x-hydra:
      upstreams:
        DB_URL: { service: db, baseline_value: "https://{host}:{tls}" }
  other:
    command: other
`))
	if err != nil {
		t.Fatal(err)
	}
	data, err := Render(st, plan.BaselinePlan(st), Options{Dir: "/repo", Router: "exec hydra router", CAFile: "/ca.pem"})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Processes map[string]struct {
			Environment []string       `yaml:"environment"`
			DependsOn   map[string]any `yaml:"depends_on"`
		} `yaml:"processes"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	web := doc.Processes["web"]
	env := strings.Join(web.Environment, "\n")
	// Main's web is given the front's address, and the CA to accept it with.
	for _, want := range []string{"DB_URL=https://127.0.0.1:5443", "NODE_EXTRA_CA_CERTS=/ca.pem", "FOO=1"} {
		if !strings.Contains(env, want) {
			t.Errorf("web env lacks %s:\n%s", want, env)
		}
	}
	// The router opens the front, so it starts first, and exists though no
	// service is routed.
	if _, ok := web.DependsOn["hydra-router"]; !ok {
		t.Errorf("web should wait for hydra-router: %v", web.DependsOn)
	}
	if _, ok := doc.Processes["hydra-router"]; !ok {
		t.Error("no hydra-router in the baseline")
	}
	// A process that names its own CA keeps it.
	if got := strings.Join(doc.Processes["job"].Environment, ","); !strings.HasPrefix(got, "NODE_EXTRA_CA_CERTS=/my/own.pem,") || strings.Contains(got, "/ca.pem") {
		t.Errorf("job env = %s", got)
	}
	// One that uses no front is left exactly as it was: adding a front to a
	// running stack must not restart everything.
	if got := strings.Join(doc.Processes["other"].Environment, ","); got != "HYDRA_SERVICE=other,HYDRA_SLOT=0,HYDRA_UPSTREAMS=" {
		t.Errorf("other env = %s", got)
	}
	if len(doc.Processes["other"].DependsOn) != 0 || len(doc.Processes["db"].DependsOn) != 0 {
		t.Errorf("processes that use no front were given dependencies")
	}
}

// process-compose builds its file watcher only when some process has a
// `watch` as the project starts. A worktree that starts with nothing of its
// own (the usual case: `hydra up`, then edit) would have none, and a service
// hydra-watch adds later would never restart on a change. So hydra-watch
// carries a watch of its own that never fires.
func TestAWorktreeAlwaysHasAWatchWhenTheStackUsesOne(t *testing.T) {
	render := func(src string, slot int, changed []string, watch string) map[string]map[string]any {
		t.Helper()
		st, err := config.Parse([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		p, err := plan.Build(st, plan.Request{Slot: slot, Changed: changed})
		if err != nil {
			t.Fatal(err)
		}
		data, err := Render(st, p, Options{Dir: "/repo", Watch: watch})
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Processes map[string]map[string]any `yaml:"processes"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Processes
	}

	// Nothing changed: hydra-watch is the only process, and it has the watch.
	procs := render(stack, 2, nil, "hydra watch")
	if _, runs := procs["api"]; runs {
		t.Fatal("api should not run in a worktree that changed nothing")
	}
	w, _ := procs["hydra-watch"]["watch"].(map[string]any)
	paths, _ := w["paths"].([]any)
	if len(paths) != 1 {
		t.Fatalf("hydra-watch should carry one watch path, got %v", procs["hydra-watch"]["watch"])
	}
	first, _ := paths[0].(map[string]any)
	if first["path"] != "/repo/"+WatchAnchor {
		t.Errorf("the watch should point at the anchor directory, got %v", first["path"])
	}
	if inc, _ := first["include"].([]any); len(inc) != 1 {
		t.Errorf("the watch needs an include that matches nothing, got %v", first["include"])
	}

	// A stack with no watch anywhere gets none.
	plain := strings.Replace(stack, "    watch: { paths: [ { path: ./api } ] }\n", "", 1)
	if _, has := render(plain, 2, nil, "hydra watch")["hydra-watch"]["watch"]; has {
		t.Error("a stack without a watch should not get one")
	}
}

// A worktree's own database answers before its migrations are in. What reads
// it waits for hydra's step, which ends when they are, instead of for the
// database's own readiness.
func TestWhatReadsAWorktreesOwnDatabaseWaitsForItToBeMigrated(t *testing.T) {
	st, err := config.Parse([]byte(`
x-hydra: { slot_stride: 200 }
processes:
  supabase:
    command: supabase start
    is_daemon: true
    readiness_probe: { http_get: { host: 127.0.0.1, port: 54321, path: /rest/v1/ } }
    x-hydra: { kind: supabase, port: 54321, ports: { db: 54322 }, paths: ["supabase/**"] }
  temporal:
    command: temporal server start-dev
    readiness_probe: { http_get: { host: 127.0.0.1, port: 8233, path: / } }
    x-hydra: { shared: true }
  api:
    command: ./api
    depends_on:
      supabase: { condition: process_healthy }
      temporal: { condition: process_healthy }
    x-hydra:
      port: 8080
      paths: ["api/**"]
      upstreams:
        DB_URL: { service: supabase, value: "postgresql://{host}:{port.db}/postgres" }
  worker:
    command: ./worker
    depends_on:
      supabase: { condition: process_healthy }
      temporal: { condition: process_healthy }
    x-hydra:
      kind: temporal
      paths: ["api/**"]
      upstreams:
        DB_URL: { service: supabase, value: "postgresql://{host}:{port.db}/postgres" }
  reader:
    command: ./reader
    x-hydra:
      paths: ["reader/**"]
      upstreams:
        DB_URL: { service: supabase, value: "postgresql://{host}:{port.db}/postgres" }
  docs:
    command: ./docs
    x-hydra: { port: 4000, paths: ["docs/**"] }
`))
	if err != nil {
		t.Fatal(err)
	}
	st.Hydra.Exe = "/bin/hydra"
	type procs map[string]struct {
		Command    string                    `yaml:"command"`
		WorkingDir string                    `yaml:"working_dir"`
		Namespace  string                    `yaml:"namespace"`
		DependsOn  map[string]map[string]any `yaml:"depends_on"`
	}
	render := func(p *plan.Plan) procs {
		t.Helper()
		out, err := Render(st, p, Options{Dir: "/wt"})
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Processes procs `yaml:"processes"`
		}
		if err := yaml.Unmarshal(out, &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Processes
	}

	p, err := plan.Build(st, plan.Request{Slot: 1, Changed: []string{"supabase/migrations/1.sql", "docs/a.md"}})
	if err != nil {
		t.Fatal(err)
	}
	got := render(p)
	step := got["hydra-ready-supabase"]
	if step.Command != "exec '/bin/hydra' prepare supabase --ready" || step.WorkingDir != "/wt" || step.Namespace != "hydra" || len(step.DependsOn) != 0 {
		t.Fatalf("the step = %+v", step)
	}
	// api and worker waited for supabase; reader only reads it. All three
	// wait for the step now, and none for supabase itself.
	for _, name := range []string{"api", "worker", "reader"} {
		deps := got[name].DependsOn
		if deps["hydra-ready-supabase"]["condition"] != "process_completed_successfully" {
			t.Errorf("%s doesn't wait for the step: %v", name, deps)
		}
		if _, still := deps["supabase"]; still {
			t.Errorf("%s still waits for supabase's own readiness: %v", name, deps)
		}
	}
	// What else they wait for is as it was.
	if got["api"].DependsOn["temporal"]["condition"] != "process_healthy" {
		t.Errorf("api lost temporal: %v", got["api"].DependsOn)
	}
	// The step that makes the worker's Temporal namespace needs Temporal,
	// not the database.
	if deps := got["hydra-prepare-worker"].DependsOn; len(deps) != 1 || deps["temporal"] == nil {
		t.Errorf("hydra-prepare-worker waits for %v", deps)
	}
	// A service that doesn't read the database waits for nothing new.
	if len(got["docs"].DependsOn) != 0 {
		t.Errorf("docs waits for %v", got["docs"].DependsOn)
	}
	if deps := got["supabase"].DependsOn; len(deps) != 0 {
		t.Errorf("supabase waits for %v", deps)
	}

	// With main's database there is no step: its readiness is the stand-in's.
	p, err = plan.Build(st, plan.Request{Slot: 1, Changed: []string{"api/main.go"}})
	if err != nil {
		t.Fatal(err)
	}
	got = render(p)
	if _, has := got["hydra-ready-supabase"]; has {
		t.Error("a step for a database that isn't this worktree's")
	}
	if got["api"].DependsOn["supabase"]["condition"] != "process_healthy" || got["supabase"].Namespace != "main" {
		t.Errorf("api = %v, supabase = %+v", got["api"].DependsOn, got["supabase"])
	}

	// Nor in main.
	if base := render(plan.BaselinePlan(st)); base["api"].DependsOn["supabase"] == nil || len(base["hydra-ready-supabase"].Command) != 0 {
		t.Errorf("main's file changed: %v", base["api"].DependsOn)
	}
}
