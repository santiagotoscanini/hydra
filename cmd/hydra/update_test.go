package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/pc"
)

// fakeCompose answers like process-compose for one process: the state before
// a restart, then one state per look after it (the last one stays).
type fakeCompose struct {
	mu        sync.Mutex
	before    pc.Process
	after     []pc.Process
	restarted bool
	looks     int
}

func (f *fakeCompose) client(t *testing.T) *pc.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/process/restart/"):
			f.restarted = true
			json.NewEncoder(w).Encode(map[string]string{"name": f.before.Name})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/process/"):
			p := f.before
			if f.restarted {
				p = f.after[min(f.looks, len(f.after)-1)]
				f.looks++
			}
			json.NewEncoder(w).Encode(p)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	return pc.TCP(host, n)
}

// process-compose builds a new state for the new run of a restarted process,
// and for one that is `disabled: true` that state reads "Disabled" (where
// another process's reads "Pending") until what it depends on is ready and
// the run begins. A voice agent started by hand in main went through exactly
// that, and the update called it a failure though it came up a moment later.
func TestARestartedProcessThatIsOffByDefaultIsWaitedFor(t *testing.T) {
	f := &fakeCompose{
		before: pc.Process{Name: "agent", Status: "Running", IsRunning: true, IsReady: "-", StartTime: "17:00:00"},
		after: []pc.Process{
			{Name: "agent", Status: "Disabled", IsReady: "-"},
			{Name: "agent", Status: "Disabled", IsReady: "-"},
			{Name: "agent", Status: "Running", IsRunning: true, IsReady: "Ready", StartTime: "17:49:31"},
		},
	}
	st := &config.Stack{Raw: map[string]any{}}
	p, err := restartAndWait(st, "agent", f.client(t), "agent", f.before, false, false, 10*time.Second)
	if err != nil {
		t.Fatalf("the restart should wait for the new run: %v", err)
	}
	if !f.restarted || p.StartTime != "17:49:31" {
		t.Fatalf("restarted=%v, ended on %+v", f.restarted, p)
	}
}

// What process-compose gives up on is "Skipped", and that is final.
func TestARestartedProcessThatIsSkippedFails(t *testing.T) {
	f := &fakeCompose{
		before: pc.Process{Name: "agent", Status: "Running", IsRunning: true, IsReady: "-", StartTime: "17:00:00"},
		after:  []pc.Process{{Name: "agent", Status: "Skipped", IsReady: "-"}},
	}
	st := &config.Stack{Raw: map[string]any{}}
	_, err := restartAndWait(st, "agent", f.client(t), "agent", f.before, false, false, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "a process it depends on failed") {
		t.Fatalf("a skipped process is a failure, got %v", err)
	}
}

// One that never starts is reported when the time is up, with what it is
// waiting for rather than a bare state.
func TestARestartedProcessThatStaysOffTimesOut(t *testing.T) {
	f := &fakeCompose{
		before: pc.Process{Name: "agent", Status: "Running", IsRunning: true, IsReady: "-", StartTime: "17:00:00"},
		after:  []pc.Process{{Name: "agent", Status: "Disabled", IsReady: "-"}},
	}
	st := &config.Stack{Raw: map[string]any{}}
	_, err := restartAndWait(st, "agent", f.client(t), "agent", f.before, false, false, time.Second)
	if err == nil || !strings.Contains(err.Error(), "still waiting for a process it depends on") {
		t.Fatalf("got %v", err)
	}
}
