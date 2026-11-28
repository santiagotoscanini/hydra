package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/santiagotoscanini/hydra/internal/journal"
)

// What went wrong: the same command failing is one entry whatever it took;
// a process the watcher restarted, or whose health check failed while it
// was busy, and that came back, is none; one that exited with an error is
// named by its exit.
func TestReportErrors(t *testing.T) {
	at := time.Date(2026, 11, 26, 17, 0, 0, 0, time.Local)
	rec := func(s int, kind string, raw map[string]any) journal.Record {
		return journal.Record{Time: at.Add(time.Duration(s) * time.Second), Kind: kind, Checkout: "wt", Raw: raw}
	}
	cmd := func(s int, ms float64) journal.Record {
		return rec(s, "command", map[string]any{"args": []any{"prepare", "supabase", "--started", "0"}, "exit": 1.0, "ms": ms})
	}
	// What the monitor writes: the output only with a failure or a restart.
	proc := func(s int, name, status, health string, exit float64) journal.Record {
		raw := map[string]any{"name": name, "status": status, "health": health, "exit": exit}
		if health == "failed" || exit != 0 {
			raw["output"] = []any{"…"}
		}
		return rec(s, "process", raw)
	}
	recs := []journal.Record{
		cmd(0, 813), cmd(7, 838), cmd(14, 796),
		proc(20, "api", "Completed", "done", -1), proc(24, "api", "Running", "starting", 0), proc(39, "api", "Running", "up", 0),
		proc(50, "supabase", "Launched", "failed", 0), proc(55, "supabase", "Launched", "up", 0),
		proc(60, "worker", "Running", "up", 1),
	}
	var b bytes.Buffer
	reportErrors(&b, recs)
	got := b.String()
	var heads []string
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "### ") {
			heads = append(heads, l)
		}
	}
	want := []string{"### process: worker exited 1", "### command: hydra prepare supabase --started 0"}
	if strings.Join(heads, "\n") != strings.Join(want, "\n") {
		t.Errorf("headings:\n%s\nwant:\n%s\n\n%s", strings.Join(heads, "\n"), strings.Join(want, "\n"), got)
	}
	if !strings.Contains(got, "3× between") {
		t.Errorf("the three failures of one command aren't counted together:\n%s", got)
	}
}
