package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/santiagotoscanini/hydra/internal/pc"
)

// A process that takes SIGTERM and never exits held `hydra down` for good,
// with nothing on the screen. Now hydra says what it waits for and kills it.
func TestDownKillsWhatWontStop(t *testing.T) {
	bin, err := exec.LookPath("process-compose")
	if err != nil {
		t.Skip("process-compose is not installed")
	}
	dir, err := os.MkdirTemp("", "hd") // short: a socket's path has a limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	file := filepath.Join(dir, "pc.yaml")
	os.WriteFile(file, []byte(`version: "0.5"
processes:
  stubborn:
    command: "trap 'echo got term' TERM; while true; do sleep 1; done"
  patient:
    command: "trap 'echo got term' TERM; while true; do sleep 1; done"
    shutdown: { timeout_seconds: 600 }
  quick:
    command: "sleep 100000"
`), 0o644)
	sock := filepath.Join(dir, "s.sock")
	up := exec.Command(bin, "up", "-D", "-f", file, "-U", "-u", sock, "-L", filepath.Join(dir, "pc.log"))
	if out, err := up.CombinedOutput(); err != nil {
		t.Fatalf("process-compose up: %v: %s", err, out)
	}
	c := pc.Unix(sock)
	t.Cleanup(func() { exec.Command(bin, "down", "-U", "-u", sock).Run() })
	for i := 0; i < 50; i++ {
		if ps, _ := c.Processes(); len(ps) == 3 && ps[0].Pid > 0 && ps[1].Pid > 0 && ps[2].Pid > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	procs, err := c.Processes()
	if err != nil {
		t.Fatal(err)
	}
	pids := map[string]int{}
	for _, p := range procs {
		pids[p.Name] = p.Pid
	}

	defer func(g, tell, poll time.Duration) { stopGrace, stopTell, stopPoll = g, tell, poll }(stopGrace, stopTell, stopPoll)
	stopGrace, stopTell, stopPoll = 2*time.Second, time.Second, 200*time.Millisecond
	grace := graceFor(c)
	if g := grace("patient"); g != 610*time.Second {
		t.Errorf("patient's own shutdown.timeout_seconds isn't respected: %s", g)
	}
	if g := grace("stubborn"); g != 2*time.Second {
		t.Errorf("stubborn gets %s", g)
	}
	// patient's own 600s is too long to wait for here: kill it by hand once
	// stubborn is dealt with, the way someone would.
	go func() {
		time.Sleep(4 * time.Second)
		exec.Command("kill", "-9", "-"+strconv.Itoa(pids["patient"])).Run()
	}()

	var said bytes.Buffer
	down := exec.Command(bin, "down", "-U", "-u", sock)
	kills, err := watchStop(down, c, &said)
	if err != nil {
		t.Fatalf("down: %v\n%s", err, said.String())
	}
	out := said.String()
	if len(kills) != 1 || kills[0].name != "stubborn" {
		t.Errorf("killed %v, want stubborn alone\n%s", kills, out)
	}
	if !strings.Contains(out, "hydra: stubborn didn't stop within") || !strings.Contains(out, "killed it (pid") {
		t.Errorf("no word of the kill:\n%s", out)
	}
	if !strings.Contains(out, "hydra: waiting for ") || !strings.Contains(out, "patient (") {
		t.Errorf("no word of what it waited for:\n%s", out)
	}
	if c.Alive() {
		time.Sleep(time.Second)
		if c.Alive() {
			t.Error("process-compose still answers after down")
		}
	}
}

func TestWaitingLine(t *testing.T) {
	for _, tc := range []struct {
		waiting, next []string
		want          string
	}{
		{[]string{"redis (40s)"}, nil, "hydra: waiting for redis (40s)"},
		{[]string{"redis (40s)"}, []string{"web", "api"}, "hydra: waiting for redis (40s); then api, web"},
		{nil, []string{"web"}, "hydra: stopping; not asked to stop yet: web"},
	} {
		if got := waitingLine(tc.waiting, tc.next); got != tc.want {
			t.Errorf("waitingLine(%v, %v) = %q, want %q", tc.waiting, tc.next, got, tc.want)
		}
	}
}
