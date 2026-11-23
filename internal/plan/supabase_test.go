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

func skews(p *Plan) string {
	var out []string
	for _, k := range p.OutOfStep {
		out = append(out, k.Service+" ("+k.Why+"): "+strings.Join(k.Files, " "))
	}
	return strings.Join(out, "; ")
}

func TestBehindMainInWhatItUsesFromMain(t *testing.T) {
	st := loadSupabase(t)
	// The branch changed web. Since it left main, main changed the api, a
	// migration, web itself, and a file no service has.
	p, err := Build(st, Request{
		Slot:        1,
		Changed:     []string{"web/page.tsx"},
		MainChanged: []string{"core/handler.go", "docs/notes.md", "supabase/migrations/2_main.sql", "web/other.tsx"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := local(p); got != "web" {
		t.Fatalf("local = %q: what main changed must not start anything here", got)
	}
	// web runs here from the branch, so main's change to web is the branch's
	// own business. The api and the database it calls are main's, newer.
	want := "api (web here uses it): core/handler.go; supabase (web here uses it): supabase/migrations/2_main.sql"
	if got := skews(p); got != want {
		t.Errorf("out of step = %q\nwant %q", got, want)
	}
}

func TestBehindMainInTheFrontendThatServesIt(t *testing.T) {
	st := loadSupabase(t)
	// The branch changed the api; main's web serves it, and main changed web.
	p, err := Build(st, Request{
		Slot:        1,
		Changed:     []string{"core/handler.go"},
		MainChanged: []string{"web/other.tsx"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := skews(p); got != "web (it serves this worktree's api): web/other.tsx" {
		t.Errorf("out of step = %q", got)
	}
	// Main changed only the api, which runs here from the branch: in step.
	p, err = Build(st, Request{Slot: 1, Changed: []string{"core/handler.go"}, MainChanged: []string{"core/other.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := skews(p); got != "" {
		t.Errorf("out of step = %q, want none", got)
	}
}

func TestNothingHereIsNeverOutOfStep(t *testing.T) {
	st := loadSupabase(t)
	// Nothing of this branch's runs, so every service it would reach is
	// main's, and those are in step with each other.
	p, err := Build(st, Request{Slot: 1, Changed: []string{"docs/x.md"}, MainChanged: []string{"core/handler.go", "web/x.tsx"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := skews(p); got != "" || local(p) != "" {
		t.Errorf("out of step = %q, local = %q", got, local(p))
	}
	if got := strings.Join(p.Unclaimed, ","); got != "docs/x.md" {
		t.Errorf("unclaimed = %q", got)
	}
}

func TestUnclaimedFilesAndIgnore(t *testing.T) {
	st, err := config.Parse([]byte(`
x-hydra: { ignore: ["docs/**", "**/*.md"] }
processes:
  api:
    command: x
    x-hydra: { port: 8080, paths: ["core/**", "!**/*_test.go"] }
`))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Build(st, Request{Slot: 1, Changed: []string{
		"core/a_test.go",    // excluded on purpose by api: taken
		"docs/guide.txt",    // ignored
		"README.md",         // ignored
		"packages/new/x.ts", // nobody's
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.Unclaimed, ","); got != "packages/new/x.ts" {
		t.Errorf("unclaimed = %q", got)
	}
}

func TestSharedJobOnFilesAnotherServiceTakes(t *testing.T) {
	// migrate lists the migrations so an update of main reruns it. In a
	// worktree those files start that worktree's own database, which applies
	// them: no warning that the shared job is skipped.
	st, err := config.Parse([]byte(`
x-hydra: { slot_stride: 200 }
processes:
  supabase:
    command: supabase start
    x-hydra: { kind: supabase, paths: ["supabase/migrations/**"] }
  migrate:
    command: supabase migration up
    x-hydra: { shared: true, paths: ["supabase/migrations/**"] }
  cache:
    command: x
    x-hydra: { shared: true, paths: ["cache/**"] }
`))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Build(st, Request{Slot: 1, Changed: []string{"supabase/migrations/1.sql", "cache/conf"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "cache is shared") {
		t.Errorf("warnings = %q, want only cache's", p.Warnings)
	}
}

func TestAJobInMainDoesNotReachAWorktreesOwnDatabase(t *testing.T) {
	st, err := config.Parse([]byte(`
x-hydra: { slot_stride: 200 }
processes:
  supabase:
    command: supabase start
    x-hydra: { kind: supabase, paths: ["supabase/migrations/**"] }
  migrate:
    command: supabase migration up
    x-hydra: { shared: true, paths: ["supabase/migrations/**"] }
  api:
    command: x
    depends_on:
      migrate: { condition: process_completed_successfully }
    x-hydra:
      port: 8080
      paths: ["core/**"]
      upstreams:
        DB: { service: supabase, value: "postgresql://{host}:{port.db}/postgres" }
`))
	if err != nil {
		t.Fatal(err)
	}
	mainChanged := []string{"supabase/migrations/9_main.sql"}
	// The branch has its own migration, so its own database: main's
	// migrations don't touch it, however far behind the branch is.
	p, err := Build(st, Request{Slot: 1, Changed: []string{"supabase/migrations/5_mine.sql"}, MainChanged: mainChanged})
	if err != nil {
		t.Fatal(err)
	}
	if got := skews(p); got != "" || local(p) != "api,supabase" {
		t.Errorf("own database: out of step = %q, local = %q", got, local(p))
	}
	// The branch changed only the api: it reads main's database, which main's
	// migrations changed since. Reported once, for the database.
	p, err = Build(st, Request{Slot: 1, Changed: []string{"core/x.go"}, MainChanged: mainChanged})
	if err != nil {
		t.Fatal(err)
	}
	want := "supabase (api here uses it): supabase/migrations/9_main.sql"
	if got := skews(p); got != want {
		t.Errorf("main's database: out of step = %q", got)
	}
}
