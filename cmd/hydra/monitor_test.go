package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/santiagotoscanini/hydra/internal/journal"
	"github.com/santiagotoscanini/hydra/internal/pc"
)

func TestTheMonitorWritesChangesAndWhyAProcessFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/process/logs/api/") {
			json.NewEncoder(w).Encode(map[string]any{"logs": []string{"panic: no database"}})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(port)

	common := t.TempDir()
	journal.Open(common, "fix-cart", 2, "test")
	m := newMonitor(pc.TCP(host, n), "/nowhere", false)
	running := pc.Process{Name: "api", Status: "Running", IsRunning: true, Pid: 10}
	web := pc.Process{Name: "web", Status: "Running", IsRunning: true, Pid: 11}
	at := time.Now()
	m.observe([]pc.Process{running, web}, at)
	m.observe([]pc.Process{running, web}, at.Add(2*time.Second)) // nothing changed
	failed := pc.Process{Name: "api", Status: "Completed", ExitCode: 2, Pid: 10}
	m.observe([]pc.Process{failed}, at.Add(4*time.Second))

	recs, err := journal.Read(common, at.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var procs []journal.Record
	usage := 0
	for _, r := range recs {
		switch r.Kind {
		case "process":
			procs = append(procs, r)
		case "usage":
			usage++
		}
	}
	if len(procs) != 4 {
		t.Fatalf("want 2 first looks, api failing and web leaving; got %d: %v", len(procs), procs)
	}
	f := procs[2]
	if f.Str("name") != "api" || f.Str("health") != "failed" || f.Str("was") != "Running" || f.Num("exit") != 2 {
		t.Errorf("the failure: %s", f.Line)
	}
	if out := strs(f.Raw["output"]); len(out) != 1 || out[0] != "panic: no database" {
		t.Errorf("the failure came without its output: %s", f.Line)
	}
	if procs[3].Str("name") != "web" || procs[3].Str("status") != "removed from the project" {
		t.Errorf("web leaving: %s", procs[3].Line)
	}
	if usage != 1 {
		t.Errorf("%d usage samples in four seconds, want 1", usage)
	}
}
