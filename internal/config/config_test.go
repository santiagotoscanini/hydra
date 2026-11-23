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
