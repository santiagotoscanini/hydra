package plan

import (
	"strings"
	"testing"

	"github.com/santiagotoscanini/hydra/internal/config"
)

const stack = `
x-hydra:
  slot_stride: 100
  global_paths: [pnpm-lock.yaml]
processes:
  db:
    command: x
    x-hydra: { port: 5000, paths: ["db/**"], url: "postgres://{host}:{port}/app" }
  api:
    command: x
    x-hydra:
      port: 5010
      paths: ["apps/api/**", "packages/shared/**"]
      upstreams: { DATABASE_URL: db }
  web-a:
    command: x
    x-hydra: { port: 5020, paths: ["apps/web-a/**"], upstreams: { API_URL: api } }
  web-b:
    command: x
    x-hydra: { port: 5021, paths: ["apps/web-b/**"], upstreams: { API_URL: api } }
  supabase:
    command: x
    x-hydra: { port: 5030, paths: ["supabase/**"], shared: true }
`

func load(t *testing.T) *config.Stack {
	t.Helper()
	st, err := config.Parse([]byte(stack))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func local(p *Plan) string { return strings.Join(p.Local(), ",") }

func upstream(t *testing.T, p *Plan, svc, env string) Upstream {
	t.Helper()
	row := p.Get(svc)
	if row == nil || !row.Local {
		t.Fatalf("%s is not local", svc)
	}
	for _, u := range row.Upstreams {
		if u.Env == env {
			return u
		}
	}
	t.Fatalf("%s has no upstream %s", svc, env)
	return Upstream{}
}

func TestChangedPathsPickServices(t *testing.T) {
	st := load(t)
	p, err := Build(st, Request{Slot: 1, Changed: []string{"apps/api/main.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := local(p); got != "api" {
		t.Fatalf("local = %q, want api", got)
	}
	u := upstream(t, p, "api", "DATABASE_URL")
	if u.Target != Baseline || u.Value != "postgres://127.0.0.1:5000/app" {
		t.Fatalf("db upstream = %+v", u)
	}
	if p.Get("api").Port != 5110 {
		t.Fatalf("api port = %d, want 5110", p.Get("api").Port)
	}
}

func TestGlobalPathsAffectEverythingButShared(t *testing.T) {
	p, err := Build(load(t), Request{Slot: 2, Changed: []string{"pnpm-lock.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := local(p); got != "api,db,web-a,web-b" {
		t.Fatalf("local = %q", got)
	}
}

func TestSharedStaysOnBaseline(t *testing.T) {
	p, err := Build(load(t), Request{Slot: 1, Changed: []string{"supabase/migrations/1.sql"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := local(p); got != "" {
		t.Fatalf("local = %q, want none", got)
	}
}

func TestLocalUpstreamByDefault(t *testing.T) {
	p, err := Build(load(t), Request{Slot: 1, Changed: []string{"apps/api/x", "apps/web-a/x"}})
	if err != nil {
		t.Fatal(err)
	}
	if u := upstream(t, p, "web-a", "API_URL"); u.Target != Local || u.Value != "http://127.0.0.1:5110" {
		t.Fatalf("web-a API_URL = %+v", u)
	}
}

func TestRouteOverrideToBaseline(t *testing.T) {
	r := Routes{}
	r.Set("web-b", "api", Baseline)
	p, err := Build(load(t), Request{
		Slot:    1,
		Changed: []string{"apps/api/x", "apps/web-a/x", "apps/web-b/x"},
		Routes:  r,
	})
	if err != nil {
		t.Fatal(err)
	}
	if u := upstream(t, p, "web-a", "API_URL"); u.Target != Local {
		t.Fatalf("web-a should use local api: %+v", u)
	}
	if u := upstream(t, p, "web-b", "API_URL"); u.Target != Baseline || u.Value != "http://127.0.0.1:5010" {
		t.Fatalf("web-b should use baseline api: %+v", u)
	}
}

func TestRouteOverrideToLocalPullsInBothEnds(t *testing.T) {
	r := Routes{}
	r.Set("web-b", "api", Local)
	p, err := Build(load(t), Request{Slot: 1, Routes: r})
	if err != nil {
		t.Fatal(err)
	}
	if got := local(p); got != "api,web-b" {
		t.Fatalf("local = %q, want api,web-b", got)
	}
	if p.Get("api").Why != "routed to from web-b" {
		t.Fatalf("api why = %q", p.Get("api").Why)
	}
}

func TestRouteToSharedFails(t *testing.T) {
	st := load(t)
	st.Services["api"].Upstreams["SUPABASE_URL"] = config.Upstream{Service: "supabase"}
	r := Routes{}
	r.Set("api", "supabase", Local)
	if _, err := Build(st, Request{Slot: 1, Routes: r}); err == nil {
		t.Fatal("want error routing to a shared service locally")
	}
}

func TestWithoutWins(t *testing.T) {
	p, err := Build(load(t), Request{Slot: 1, Changed: []string{"packages/shared/x"}, Without: []string{"api"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := local(p); got != "" {
		t.Fatalf("local = %q, want none", got)
	}
}

func TestBadInputs(t *testing.T) {
	st := load(t)
	if _, err := Build(st, Request{Slot: 0}); err == nil {
		t.Error("slot 0 should be rejected")
	}
	if _, err := Build(st, Request{Slot: 1, With: []string{"nope"}}); err == nil {
		t.Error("unknown --with should be rejected")
	}
	r := Routes{}
	r.Set("web-a", "db", Local)
	if _, err := Build(st, Request{Slot: 1, Routes: r}); err == nil {
		t.Error("route to a non-upstream should be rejected")
	}
}

func TestExclusions(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"services/core/handler/x.go", true},
		{"services/core/handler/x_test.go", false},
		{"services/core/testdata/a.json", false},
	}
	pats := []string{"services/core/**", "!**/*_test.go", "!**/testdata/**"}
	for _, c := range cases {
		if _, got := firstMatch(pats, []string{c.path}); got != c.want {
			t.Errorf("%s: got %v, want %v", c.path, got, c.want)
		}
	}
}

func TestLayersPickTheTopmostOwner(t *testing.T) {
	st, err := config.Parse([]byte(`
processes:
  deps:
    command: install
    x-hydra: { follow: true }
  api:
    command: api
    depends_on: { deps: { condition: process_completed_successfully } }
    x-hydra: { port: 5010, paths: ["api/**"] }
  web:
    command: web
    depends_on: { deps: { condition: process_completed_successfully } }
    x-hydra: { port: 5020, paths: ["web/**"], upstreams: { API_URL: api } }
`))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Build(st, Request{Slot: 1, Layers: []Layer{
		{Name: "b1", Root: "/wt/b1", Changed: []string{"api/a.go"}},
		{Name: "b2", Root: "/wt/b2", Changed: []string{"api/b.go"}}, // edits api again on top
		{Name: "b3", Root: "/wt/b3", Changed: []string{"web/page.tsx"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := local(p); got != "api,deps,deps-b2,web" {
		t.Fatalf("local = %q", got)
	}
	if api := p.Get("api"); api.Root != "/wt/b2" || api.Why != "changed in b2: api/b.go" {
		t.Errorf("api should run from b2: %+v", api)
	}
	if web := p.Get("web"); web.Root != "/wt/b3" {
		t.Errorf("web should run from b3: %+v", web)
	}
	if d := p.Get("deps-b2"); d.Root != "/wt/b2" || d.Source != "deps" {
		t.Errorf("deps copy for b2: %+v", d)
	}
	if p.Get("api").FollowDeps["deps"] != "deps-b2" {
		t.Errorf("api should wait for its own worktree's deps: %v", p.Get("api").FollowDeps)
	}
	if d := p.Get("deps"); d.Root != "/wt/b3" {
		t.Errorf("deps for the top worktree: %+v", d)
	}
	if u := upstream(t, p, "web", "API_URL"); u.Target != Local {
		t.Errorf("web should use the stack's api: %+v", u)
	}
}

const routedStack = `
x-hydra:
  slot_stride: 100
  max_slots: 5
processes:
  api:
    command: x
    x-hydra: { port: 6010, paths: ["apps/api/**"] }
  worker:
    command: x
    x-hydra: { paths: ["apps/worker/**"], url: "hydra-{slot}", baseline_url: default }
  web:
    command: x
    x-hydra:
      port: 6000
      paths: ["apps/web/**"]
      routed: node
      upstreams: { API_URL: api, QUEUE: worker, SITE_URL: web }
  mobile:
    command: x
    x-hydra: { paths: ["apps/mobile/**"], upstreams: { WEB_PORT: { service: web, value: "{port}" } } }
`

func loadRouted(t *testing.T, edit func(string) string) *config.Stack {
	t.Helper()
	src := routedStack
	if edit != nil {
		src = edit(src)
	}
	st, err := config.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func routedUpstream(t *testing.T, p *Plan, svc, env string) Upstream {
	t.Helper()
	row := p.Get(svc)
	if row == nil || !row.Routed {
		t.Fatalf("%s is not routed: %+v", svc, row)
	}
	for _, u := range row.Upstreams {
		if u.Env == env {
			return u
		}
	}
	t.Fatalf("routed %s has no upstream %s", svc, env)
	return Upstream{}
}

func TestRoutedServiceStaysOnMainsCopy(t *testing.T) {
	st := loadRouted(t, nil)

	// The api changed: only the api runs here. Web is main's copy behind
	// this worktree's port, and its api calls are routed here per request.
	p, err := Build(st, Request{Slot: 2, Changed: []string{"apps/api/list.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := local(p); got != "api" {
		t.Errorf("local = %q, want api", got)
	}
	web := p.Get("web")
	if !web.Routed || web.Local || web.Port != 6200 {
		t.Errorf("web = %+v, want routed on :6200", web)
	}
	if u := routedUpstream(t, p, "web", "API_URL"); u.Target != Local || u.Value != "http://127.0.0.1:6210" {
		t.Errorf("web API_URL = %+v", u)
	}

	// Nothing changed: web is still reachable on the worktree's port, and
	// its calls go to main's api.
	p, err = Build(st, Request{Slot: 2, Changed: []string{"README.md"}})
	if err != nil {
		t.Fatal(err)
	}
	if u := routedUpstream(t, p, "web", "API_URL"); u.Target != Baseline || u.Value != "http://127.0.0.1:6010" {
		t.Errorf("web API_URL = %+v", u)
	}

	// Web itself changed: it runs here, like any service.
	p, err = Build(st, Request{Slot: 2, Changed: []string{"apps/web/page.tsx"}})
	if err != nil {
		t.Fatal(err)
	}
	if web := p.Get("web"); !web.Local || web.Routed {
		t.Errorf("web = %+v, want local", web)
	}
}

func TestRoutedServiceRunsHereWhenACallCantBeRouted(t *testing.T) {
	// The worker changed, and web names its queue in an env var read once at
	// start. Main's copy would keep using main's queue, so web runs here.
	st := loadRouted(t, nil)
	p, err := Build(st, Request{Slot: 1, Changed: []string{"apps/worker/job.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := local(p); got != "web,worker" {
		t.Fatalf("local = %q, want web,worker", got)
	}
	if why := p.Get("web").Why; !strings.Contains(why, "QUEUE points at worker here and can't be routed per request") {
		t.Errorf("web why = %q", why)
	}

	// Once web picks the queue per request, main's copy can serve it.
	st = loadRouted(t, func(s string) string {
		return strings.Replace(s, "QUEUE: worker", "QUEUE: { service: worker, per_request: true }", 1)
	})
	p, err = Build(st, Request{Slot: 1, Changed: []string{"apps/worker/job.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := local(p); got != "worker" {
		t.Fatalf("local = %q, want worker", got)
	}
	if u := routedUpstream(t, p, "web", "QUEUE"); u.Value != "hydra-1" || !u.PerRequest {
		t.Errorf("web QUEUE = %+v", u)
	}

	// Excluded on purpose: it stays on main's copy, with a warning.
	st = loadRouted(t, nil)
	p, err = Build(st, Request{Slot: 1, Changed: []string{"apps/worker/job.go"}, Without: []string{"web"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := local(p); got != "worker" || len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "web is excluded") {
		t.Errorf("local = %q, warnings = %q", got, p.Warnings)
	}
}

func TestCallsIntoARoutedServiceUseTheWorktreesPort(t *testing.T) {
	// Mobile runs here and calls web, which doesn't: it goes through this
	// worktree's port for web, so its requests carry the worktree's tag.
	st := loadRouted(t, nil)
	p, err := Build(st, Request{Slot: 3, Changed: []string{"apps/mobile/app.tsx"}})
	if err != nil {
		t.Fatal(err)
	}
	if u := upstream(t, p, "mobile", "WEB_PORT"); u.Target != Routed || u.Value != "6300" {
		t.Errorf("mobile WEB_PORT = %+v", u)
	}
}

func TestBaselineSendsRoutedCallsThroughTheRouter(t *testing.T) {
	st := loadRouted(t, nil)
	p := BaselinePlan(st)
	web := p.Get("web")
	got := map[string]string{}
	for _, u := range web.Upstreams {
		got[u.Env] = u.Value
	}
	// The api is one stride past the last slot; the queue name and web's own
	// address are left alone.
	want := map[string]string{"API_URL": "http://127.0.0.1:6610", "QUEUE": "default", "SITE_URL": "http://127.0.0.1:6000"}
	for env, v := range want {
		if got[env] != v {
			t.Errorf("%s = %q, want %q", env, got[env], v)
		}
	}
	if strings.Join(web.Dispatch, ",") != "API_URL" {
		t.Errorf("dispatch = %v", web.Dispatch)
	}
}

func TestTemporalKindPreparesItsNamespace(t *testing.T) {
	st, err := config.Parse([]byte(`
x-hydra: { slot_stride: 100 }
processes:
  worker:
    command: x
    x-hydra: { kind: temporal, paths: ["core/**"] }
  web:
    command: x
    x-hydra: { port: 3000, paths: ["web/**"], routed: node, upstreams: { TEMPORAL_NAMESPACE: worker } }
`))
	if err != nil {
		t.Fatal(err)
	}
	st.Hydra.Exe = "/opt/my tools/hydra"
	p, err := Build(st, Request{Slot: 2, Changed: []string{"core/job.go"}})
	if err != nil {
		t.Fatal(err)
	}
	// The worker gets its namespace made first; web stays on main's copy and
	// switches it per request.
	if got, want := p.Get("worker").Prepare, `'/opt/my tools/hydra' prepare temporal --namespace hydra-2 --from default`; got != want {
		t.Errorf("prepare = %q, want %q", got, want)
	}
	if got := local(p); got != "worker" {
		t.Errorf("local = %q, want worker", got)
	}
	if u := routedUpstream(t, p, "web", "TEMPORAL_NAMESPACE"); u.Value != "hydra-2" || !u.PerRequest {
		t.Errorf("web TEMPORAL_NAMESPACE = %+v", u)
	}
}
