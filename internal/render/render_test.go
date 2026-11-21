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
	if db["namespace"] != "baseline" || db["shutdown"] != nil || db["is_daemon"] != nil {
		t.Errorf("db stand-in must only wait: %v", db)
	}
	if db["readiness_probe"].(map[string]any)["http_get"].(map[string]any)["port"] != 5432 {
		t.Errorf("db stand-in probe should watch the baseline port: %v", db)
	}
	if doc.Processes["migrate"]["command"] != "true" {
		t.Errorf("migrate stand-in should complete at once: %v", doc.Processes["migrate"])
	}
	if doc.Processes["deps"]["namespace"] == "baseline" {
		t.Errorf("deps follows api, it should run for real")
	}
	if doc.Processes["hydra-prepare-api"]["command"] != "setup http://127.0.0.1:10080" {
		t.Errorf("prepare = %v", doc.Processes["hydra-prepare-api"])
	}
	if _, ok := api["depends_on"].(map[string]any)["hydra-prepare-api"]; !ok {
		t.Errorf("api must wait for its prepare step")
	}
}
