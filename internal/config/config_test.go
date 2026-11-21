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
		"x-hydra: { kind: rabbit }":                 "unknown kind \"rabbit\" (known: temporal)",
		"x-hydra: { kind: temporal, shared: true }": "can't be shared, follow or routed",
	} {
		_, err := Parse([]byte("processes:\n  w:\n    command: x\n    " + src + "\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", src, err, want)
		}
	}
}
