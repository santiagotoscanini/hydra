package config

import (
	"strings"
	"testing"
)

const routed = `
x-hydra: { slot_stride: 100, max_slots: 5 }
processes:
  db:
    command: x
    x-hydra: { port: 7030, url: "postgres://{host}:{port}/app" }
  api:
    command: x
    x-hydra: { port: 7010, upstreams: { DATABASE_URL: db } }
  worker:
    command: x
    x-hydra: { url: "queue-{slot}" }
  web:
    command: x
    x-hydra:
      port: 7000
      routed: node
      upstreams:
        API_URL: api
        API_PORT: { service: api, value: "{port}" }
        DATABASE_URL: db
        QUEUE: { service: worker, per_request: true }
`

func TestRoutedConfig(t *testing.T) {
	st, err := Parse([]byte(routed))
	if err != nil {
		t.Fatal(err)
	}
	web := st.Services["web"]
	if web.Routed != RoutedNode || !st.HasRouted() {
		t.Fatalf("web.Routed = %q", web.Routed)
	}
	// A proxy can route an HTTP URL or a bare port, not a database URL or a
	// queue name.
	for env, want := range map[string]bool{"API_URL": true, "API_PORT": true, "DATABASE_URL": false, "QUEUE": false} {
		if got := st.Routable(web.Upstreams[env]); got != want {
			t.Errorf("Routable(%s) = %v, want %v", env, got, want)
		}
	}
	if got := strings.Join(st.Dispatched(), ","); got != "api" {
		t.Errorf("Dispatched = %q, want api", got)
	}
	if got := st.DispatchPort(st.Services["api"]); got != 7610 {
		t.Errorf("DispatchPort(api) = %d, want 7610", got)
	}
}

func TestRoutedValidation(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"needs a port":      {"port: 7000\n      routed: node", "routed: node", "routed needs x-hydra.port"},
		"not with shared":   {"routed: node", "routed: node\n      shared: true", "can't be combined with shared"},
		"unknown value":     {"routed: node", "routed: python", "want true or node"},
		"per_request needs": {"x-hydra: { port: 7010, upstreams: { DATABASE_URL: db } }", "x-hydra: { port: 7010, upstreams: { DATABASE_URL: { service: db, per_request: true } } }", "only applies to a routed service"},
		"router port clash": {"x-hydra: { url: \"queue-{slot}\" }", "x-hydra: { port: 7610, url: \"queue-{slot}\" }", "the router's port for api collides"},
	}
	for name, c := range cases {
		if !strings.Contains(routed, c.from) {
			t.Fatalf("%s: fixture has no %q", name, c.from)
		}
		_, err := Parse([]byte(strings.Replace(routed, c.from, c.to, 1)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, c.want)
		}
	}
}

func TestTemporalKind(t *testing.T) {
	st, err := Parse([]byte(`
processes:
  worker:
    command: x
    x-hydra: { kind: temporal, paths: ["core/**"] }
  api:
    command: x
    x-hydra: { port: 8080, upstreams: { TEMPORAL_NAMESPACE: worker } }
  web:
    command: x
    x-hydra: { port: 3000, routed: node, upstreams: { TEMPORAL_NAMESPACE: worker } }
`))
	if err != nil {
		t.Fatal(err)
	}
	w := st.Services["worker"]
	if w.URL != "hydra-{slot}" || w.BaselineURL != "default" {
		t.Errorf("worker url = %q, baseline_url = %q", w.URL, w.BaselineURL)
	}
	// Only a routed service switches it per request; the api reads it once.
	if !st.Services["web"].Upstreams["TEMPORAL_NAMESPACE"].PerRequest || st.Services["api"].Upstreams["TEMPORAL_NAMESPACE"].PerRequest {
		t.Error("per_request should be set on web's upstream only")
	}

	for src, want := range map[string]string{
		"x-hydra: { kind: rabbit }":                 "unknown kind \"rabbit\" (known: temporal, supabase)",
		"x-hydra: { kind: temporal, shared: true }": "can't be shared, follow or routed",
		"x-hydra: { seed_from_main: true }":         "seed_from_main is for kind: supabase",
	} {
		_, err := Parse([]byte("processes:\n  w:\n    command: x\n    " + src + "\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", src, err, want)
		}
	}
}

func TestSupabaseKind(t *testing.T) {
	st, err := Parse([]byte(`
x-hydra: { slot_stride: 200 }
processes:
  supabase:
    command: supabase start
    x-hydra: { kind: supabase, paths: ["supabase/**"] }
  api:
    command: x
    x-hydra:
      port: 8080
      upstreams: { DB: { service: supabase, value: "postgres://{host}:{port.db}/x" } }
`))
	if err != nil {
		t.Fatal(err)
	}
	s := st.Services["supabase"]
	if s.Port != 54321 || s.Ports["db"] != 54322 {
		t.Fatalf("defaults: port %d, db %d", s.Port, s.Ports["db"])
	}
	ports := st.SlotPorts(s, 1)
	if ports[""] != 54521 || ports["db"] != 54522 || ports["db_shadow"] != 54520 || ports["edge_runtime_inspector"] != 54519 {
		t.Errorf("slot 1 ports = %v", ports)
	}
	if len(st.SlotPorts(s, 0)) != 2 {
		t.Errorf("the baseline's stack is the user's own; hydra tracks only its API and db: %v", st.SlotPorts(s, 0))
	}
	env := strings.Join(st.SupabaseEnv(s, 2), " ")
	for _, want := range []string{"SUPABASE_EXPERIMENTAL_STACK=1", "SUPABASE_API_PORT=54721", "SUPABASE_DB_PORT=54722", "SUPABASE_EDGE_RUNTIME_INSPECTOR_PORT=54719"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s: %s", want, env)
		}
	}
}

func TestNamedPortValidation(t *testing.T) {
	for in, want := range map[string]string{
		// A placeholder for a port the target doesn't declare.
		`processes:
  db: { command: x, x-hydra: { port: 5432 } }
  api: { command: x, x-hydra: { port: 80, upstreams: { U: { service: db, value: "{port.admin}" } } } }`: "no x-hydra.ports.admin",
		// Slot ports landing in the range the Supabase CLI keeps for itself.
		`x-hydra: { slot_stride: 1000 }
processes:
  supabase: { command: x, x-hydra: { kind: supabase, port: 9000 } }`: "keeps for its own services",
		// Another service's slot port inside a Supabase stack's range.
		`x-hydra: { slot_stride: 200 }
processes:
  supabase: { command: x, x-hydra: { kind: supabase } }
  other: { command: x, x-hydra: { port: 54328 } }`: "collides",
	} {
		_, err := Parse([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s\ngot %v, want %q", in, err, want)
		}
	}
}

// worktree_port moves a service's worktree ports to another base, named
// ports and a Supabase stack's own with it; main keeps its port.
func TestWorktreePortMovesEveryWorktreePort(t *testing.T) {
	st, err := Parse([]byte(`
x-hydra: { slot_stride: 200, max_slots: 20 }
processes:
  supabase: { command: x, x-hydra: { kind: supabase, port: 54321, ports: { db: 54322 }, worktree_port: 40321 } }
  api: { command: x, x-hydra: { port: 8080 } }
`))
	if err != nil {
		t.Fatal(err)
	}
	sb := st.Services["supabase"]
	for slot, want := range map[int]map[string]int{
		0:  {"": 54321, "db": 54322},
		1:  {"": 40321, "db": 40322, "studio": 40323},
		20: {"": 44121, "db": 44122, "studio": 44123},
	} {
		got := st.SlotPorts(sb, slot)
		for k, v := range want {
			if got[k] != v {
				t.Errorf("slot %d port %q = %d, want %d", slot, k, got[k], v)
			}
		}
	}
	env := strings.Join(st.SupabaseEnv(sb, 2), " ")
	for _, want := range []string{"SUPABASE_API_PORT=40521", "SUPABASE_DB_PORT=40522"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s: %s", want, env)
		}
	}
	if got := st.SlotPorts(st.Services["api"], 2)[""]; got != 8480 {
		t.Errorf("api in slot 2 = %d, want 8480 (no worktree_port)", got)
	}

	// The moved ports are checked like any others.
	_, err = Parse([]byte(`
x-hydra: { slot_stride: 200, max_slots: 20 }
processes:
  supabase: { command: x, x-hydra: { kind: supabase, port: 54321, worktree_port: 40321 } }
  other: { command: x, x-hydra: { port: 41128 } }
`))
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Errorf("a collision with a moved port: got %v", err)
	}
}

func TestSlotsWhereABrowserRefusesAPortAreBad(t *testing.T) {
	st, err := Parse([]byte(`
x-hydra: { slot_stride: 200, max_slots: 20 }
processes:
  web: { command: x, x-hydra: { port: 3000 } }
  api: { command: x, x-hydra: { port: 8080 } }
  db: { command: x, x-hydra: { shared: true, port: 6665 } }
`))
	if err != nil {
		t.Fatal(err)
	}
	for slot, want := range map[int]string{0: "", 1: "", 10: "api on 10080", 15: "web on 6000", 20: ""} {
		if got := st.BadSlot(slot); got != want {
			t.Errorf("slot %d: %q, want %q", slot, got, want)
		}
	}
}

func TestTLSFronts(t *testing.T) {
	st, err := Parse([]byte(`
x-hydra: { slot_stride: 100, max_slots: 5 }
processes:
  db:
    command: db
    x-hydra: { shared: true, port: 5432, tls: 5443 }
  queue:
    command: queue
    x-hydra: { shared: true, port: 7233, tls: { port: 7243, alpn: h2 } }
  web:
    command: web
    x-hydra:
      port: 3000
      upstreams:
        DB_URL: { service: db, baseline_value: "https://{host}:{tls}" }
`))
	if err != nil {
		t.Fatal(err)
	}
	if f := st.Services["db"].TLS; f == nil || f.Port != 5443 || f.ALPN != "http/1.1" {
		t.Errorf("db tls = %+v, want :5443 http/1.1", f)
	}
	if f := st.Services["queue"].TLS; f == nil || f.Port != 7243 || f.ALPN != "h2" {
		t.Errorf("queue tls = %+v", f)
	}
	if !st.HasTLS() {
		t.Error("HasTLS should be true")
	}

	for name, tc := range map[string]struct{ yaml, want string }{
		"a front needs the port it passes to": {
			`processes: { db: { command: x, x-hydra: { shared: true, tls: 5443 } } }`, "tls needs x-hydra.port"},
		"only the two protocols": {
			`processes: { db: { command: x, x-hydra: { port: 5432, tls: { port: 5443, alpn: spdy } } } }`, "want http/1.1 or h2"},
		"a worktree's copy has no front": {
			`processes:
  db: { command: x, x-hydra: { port: 5432, tls: 5443 } }
  web: { command: x, x-hydra: { upstreams: { DB: { service: db, value: "https://{host}:{tls}" } } } }`, "belongs in baseline_value"},
		"{tls} needs a front": {
			`processes:
  db: { command: x, x-hydra: { port: 5432 } }
  web: { command: x, x-hydra: { upstreams: { DB: { service: db, baseline_value: "https://{host}:{tls}" } } } }`, "has no x-hydra.tls"},
		"a front's port is taken like any other": {
			`x-hydra: { slot_stride: 100, max_slots: 3 }
processes:
  db: { command: x, x-hydra: { port: 5432, tls: 5532 } }`, "collides with"},
	} {
		if _, err := Parse([]byte(tc.yaml)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestPool(t *testing.T) {
	st, err := Parse([]byte(`
x-hydra: { slot_stride: 200 }
processes:
  supabase:
    command: x
    x-hydra: { kind: supabase, pool: 6432 }
`))
	if err != nil {
		t.Fatal(err)
	}
	s := st.Pooled()
	if s == nil || s.Name != "supabase" {
		t.Fatalf("Pooled = %v", s)
	}
	p := s.Pool
	if p.Port != 6432 || p.Of != "db" || p.Size != 20 {
		t.Errorf("defaults: %+v", *p)
	}
	if got := st.PoolTarget(s); got != 54322 {
		t.Errorf("PoolTarget = %d, want 54322", got)
	}

	for in, want := range map[string]string{
		`processes:
  db: { command: x, x-hydra: { port: 5432, pool: { size: 5 } } }`: "needs a port of its own",
		`processes:
  db: { command: x, x-hydra: { pool: 6432 } }`: "needs x-hydra.port",
		`processes:
  db: { command: x, x-hydra: { port: 5432, pool: { port: 6432, of: admin } } }`: `no port named "admin"`,
		`processes:
  a: { command: x, x-hydra: { port: 5432, pool: 6432 } }
  b: { command: x, x-hydra: { port: 5433, pool: 6433 } }`: "only one service",
		`x-hydra: { slot_stride: 100 }
processes:
  db: { command: x, x-hydra: { port: 5432, pool: 5532 } }`: "collides",
	} {
		_, err := Parse([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s\ngot %v, want %q", in, err, want)
		}
	}
}
