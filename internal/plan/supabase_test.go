package plan

import (
	"strings"
	"testing"

	"github.com/santiagotoscanini/hydra/internal/config"
)

// The monorepo's shape: a Supabase kind, a Go api and worker reading its
// database, a routed Next app calling it from the browser and over SQL.
const supabaseStack = `
x-hydra: { slot_stride: 200 }
processes:
  supabase:
    command: supabase start
    x-hydra:
      kind: supabase
      ports: { db: 54322 }
      paths: ["supabase/migrations/**"]
  api:
    command: x
    x-hydra:
      port: 8080
      paths: ["core/**"]
      upstreams:
        SUPABASE_API_URL: supabase
        SUPABASE_URL: { service: supabase, value: "postgresql://postgres@{host}:{port.db}/postgres" }
  web:
    command: x
    x-hydra:
      port: 3000
      paths: ["web/**"]
      routed: node
      upstreams:
        GO_BACKEND_URL: api
        NEXT_PUBLIC_SUPABASE_URL: supabase
        SQL_URL: { service: supabase, value: "postgresql://reader@{host}:{port.db}/postgres" }
        COOKIE: { service: supabase, value: "sb-hydra-{slot}" }
`

func loadSupabase(t *testing.T) *config.Stack {
	t.Helper()
	st, err := config.Parse([]byte(supabaseStack))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestWorktreeWithItsOwnSupabaseRunsWhatReadsIt(t *testing.T) {
	st := loadSupabase(t)
	p, err := Build(st, Request{Slot: 2, Changed: []string{"supabase/migrations/1_x.sql"}})
	if err != nil {
		t.Fatal(err)
	}
	// api reads the database over SQL: main's copy would read main's. web
	// does too, so it can't stay on main's routed copy either.
	if got := local(p); got != "api,supabase,web" {
		t.Fatalf("local = %q, want api,supabase,web", got)
	}
	if got := p.Get("api").Why; !strings.Contains(got, "SUPABASE_URL points at supabase, which runs here") {
		t.Errorf("api why = %q", got)
	}
	for env, want := range map[string]string{
		"SUPABASE_API_URL": "http://127.0.0.1:54721",
		"SUPABASE_URL":     "postgresql://postgres@127.0.0.1:54722/postgres",
	} {
		if u := upstream(t, p, "api", env); u.Value != want {
			t.Errorf("api %s = %q, want %q", env, u.Value, want)
		}
	}
	if u := upstream(t, p, "web", "COOKIE"); u.Value != "sb-hydra-2" {
		t.Errorf("web COOKIE = %q", u.Value)
	}
	if p.Get("supabase").Port != 54721 {
		t.Errorf("supabase port = %d", p.Get("supabase").Port)
	}
}

func TestWorktreeOnMainsSupabaseKeepsMainsWeb(t *testing.T) {
	st := loadSupabase(t)
	p, err := Build(st, Request{Slot: 2, Changed: []string{"core/handler.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := local(p); got != "api" {
		t.Fatalf("local = %q, want api", got)
	}
	if u := upstream(t, p, "api", "SUPABASE_URL"); u.Value != "postgresql://postgres@127.0.0.1:54322/postgres" {
		t.Errorf("api SUPABASE_URL = %q", u.Value)
	}
	// main's web only routes its API calls; Supabase it reaches directly.
	web := p.Get("web")
	if !web.Routed {
		t.Fatal("web should be main's routed copy")
	}
	for _, u := range web.Upstreams {
		if u.Service == "supabase" {
			t.Errorf("web lists %s as switched per request", u.Env)
		}
	}
}

func TestBaselineReachesSupabaseDirectly(t *testing.T) {
	st := loadSupabase(t)
	// web also reaches supabase over SQL, which can't be routed, so its HTTP
	// calls there don't go through the router either.
	if got := strings.Join(st.Dispatched(), ","); got != "api" {
		t.Errorf("dispatched = %q, want api", got)
	}
	p := BaselinePlan(st)
	for _, u := range p.Get("web").Upstreams {
		if u.Env == "NEXT_PUBLIC_SUPABASE_URL" && u.Value != "http://127.0.0.1:54321" {
			t.Errorf("baseline NEXT_PUBLIC_SUPABASE_URL = %q", u.Value)
		}
	}
	if got := strings.Join(p.Get("web").Dispatch, ","); got != "GO_BACKEND_URL" {
		t.Errorf("baseline dispatch = %q", got)
	}
}

func TestAnyServiceFollowsADatabaseRunningHere(t *testing.T) {
	// The base fixture: db is plain (no kind), api reaches it by URL.
	p, err := Build(load(t), Request{Slot: 1, Changed: []string{"db/schema.sql"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := local(p); got != "api,db" {
		t.Errorf("local = %q, want api,db (web-a and web-b call api over HTTP and stay)", got)
	}
}

func TestWorktreeBehindMainsSchemaGetsItsOwnSupabase(t *testing.T) {
	st := loadSupabase(t)
	// It changed only Go code, but main gained a migration since it branched.
	p, err := Build(st, Request{
		Slot:         1,
		Changed:      []string{"core/handler.go"},
		BaselineDiff: []string{"core/handler.go", "supabase/migrations/2_main.sql"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Get("supabase").Why; got != "differs from the main checkout's: supabase/migrations/2_main.sql" {
		t.Errorf("supabase why = %q", got)
	}
	if got := local(p); got != "api,supabase,web" {
		t.Errorf("local = %q", got)
	}
	// Code that differs only because main moved stays on the baseline.
	p, err = Build(st, Request{Slot: 1, BaselineDiff: []string{"core/handler.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := local(p); got != "" {
		t.Errorf("local = %q, want nothing", got)
	}
}
