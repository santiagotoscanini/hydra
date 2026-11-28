package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/pc"
	"github.com/santiagotoscanini/hydra/internal/plan"
	"github.com/santiagotoscanini/hydra/internal/routes"
	"github.com/santiagotoscanini/hydra/internal/tunnels"
)

const tunnelStack = `
x-hydra: { slot_stride: 100, max_slots: 9 }
processes:
  api:
    command: x
    x-hydra: { port: 7010 }
  tunnel:
    command: sleep 100000
    x-hydra: { tunnel: { to: api, port: 7079, url: https://hooks.example.com } }
`

func init() {
	// No test asks the real address; TestTunnelCheck swaps in its own.
	tunnelCheck = func(v tunnelView) (string, int, error) { return v.routerTunnel().Label, 200, nil }
}

// The check through the public address: through hydra-router, around it,
// a tunnel that isn't connected, and one that can't be asked.
func TestTunnelCheck(t *testing.T) {
	defer func(f func(tunnelView) (string, int, error)) { tunnelCheck = f }(tunnelCheck)
	v := tunnelView{Name: "tunnel", URL: "https://hooks.example.com", To: "api", Port: 7079}
	cases := []struct {
		by   string
		code int
		err  error
		want string
		bad  bool
	}{
		{"api@fix", 200, nil, "a request to https://hooks.example.com came through hydra-router to api@fix", false},
		{"", 404, nil, "✗ requests to https://hooks.example.com don't come through hydra-router: something else answered (HTTP 404)", true},
		{"", 530, nil, "✗ https://hooks.example.com isn't connected (HTTP 530 from the tunnel's provider): is main's tunnel process running?", true},
		{"", 0, os.ErrDeadlineExceeded, "couldn't ask https://hooks.example.com whether it comes through hydra-router", false},
	}
	for _, c := range cases {
		tunnelCheck = func(tunnelView) (string, int, error) { return c.by, c.code, c.err }
		got, bad := checkTunnel(v)
		if !strings.HasPrefix(got, c.want) || bad != c.bad {
			t.Errorf("%q %d %v: %q %v", c.by, c.code, c.err, got, bad)
		}
	}
	if got, _ := checkTunnel(tunnelView{Name: "tunnel", To: "api"}); got != "" {
		t.Errorf("no url, nothing to ask: %q", got)
	}
	tunnelCheck = func(tunnelView) (string, int, error) { return "", 404, nil }
	var out bytes.Buffer
	if !printTunnels(&out, []tunnelView{v}) || !strings.Contains(out.String(), "  checked  ✗ requests to") {
		t.Errorf("hydra tunnel said:\n%s", out.String())
	}
}

// Where a tunnel's requests go: main's api; a holder's own api, with its
// tag; main's api with the holder's tag when the holder runs none; and
// nowhere when the holder's stack isn't running.
func TestTunnelViews(t *testing.T) {
	st, err := config.Parse([]byte(tunnelStack))
	if err != nil {
		t.Fatal(err)
	}
	files := []routes.File{
		{Slot: 1, Root: "/wt/own", Local: map[string]int{"api": 7110}},
		{Slot: 2, Root: "/wt/none", Local: map[string]int{}},
		{Slot: 3, Root: "/wt/stopped", Local: map[string]int{"api": 7310}},
	}
	live := func(f routes.File) bool { return f.Root != "/wt/stopped" }
	view := func(root string) tunnelView {
		held := map[string]tunnels.Holder{}
		if root != "" {
			held["tunnel"] = tunnels.Holder{Root: root, Since: time.Now()}
		}
		v := tunnelViews(st, held, files, live)
		if len(v) != 1 {
			t.Fatalf("views = %+v", v)
		}
		return v[0]
	}
	cases := []struct {
		root, target, label, tag string
		own, down                bool
	}{
		{"", "main:7010", "api@main", "", false, false},
		{"/wt/own", "own:7110", "api@own", "1", true, false},
		{"/wt/none", "main:7010 for none", "api@main for none", "2", false, false},
		{"/wt/stopped", "stopped (stack not running)", "", "", false, true},
		{"/wt/never-started", "never-started (stack not running)", "", "", false, true},
	}
	for _, c := range cases {
		v := view(c.root)
		rt := v.routerTunnel()
		if v.Target != c.target || (c.label != "" && rt.Label != c.label) || rt.Tag != c.tag || rt.Own != c.own || (rt.Down != "") != c.down || rt.Listen != 7079 {
			t.Errorf("held by %q: %+v -> %+v", c.root, v, rt)
		}
	}
	if v := view("/wt/stopped"); !strings.Contains(v.Down, "worktree stopped holds tunnel tunnel, and its stack isn't running") {
		t.Errorf("down = %q", v.Down)
	}
}

// take in a worktree, take in main (back to main), release, and a reap of
// the holder's slot, which gives the tunnel back too.
func TestTakeAndReleaseATunnel(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	co := gitRepo(t)
	os.WriteFile(filepath.Join(co.Root, "process-compose.yaml"), []byte(tunnelStack), 0o644)
	st := mainStackOf(co)
	if st == nil {
		t.Fatal("no stack")
	}
	e := slotted(t, co, "hooks-wt")
	wt := e.Root
	wco := co
	wco.Root = wt

	var out bytes.Buffer
	if err := takeTunnel(wco, st, "tunnel", &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tunnel tunnel (https://hooks.example.com) now comes here; main had it",
		"this worktree's stack isn't running, so its requests get a 502", "give it back with: hydra tunnel release\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("take said:\n%s\nwant %q", out.String(), want)
		}
	}
	held, _ := tunnels.Read(co.CommonDir)
	if held["tunnel"].Root != wt || held["tunnel"].Branch != "hooks-wt" {
		t.Fatalf("held = %+v", held)
	}

	out.Reset()
	takeTunnel(wco, st, "tunnel", &out)
	if !strings.Contains(out.String(), "already comes here") {
		t.Errorf("second take said %q", out.String())
	}

	out.Reset()
	if err := releaseTunnel(co, st, "tunnel", &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "tunnel tunnel (https://hooks.example.com) goes to main's api again; hooks-wt had it\n" {
		t.Errorf("release said %q", got)
	}
	out.Reset()
	releaseTunnel(co, st, "tunnel", &out)
	if !strings.Contains(out.String(), "already goes to main") {
		t.Errorf("second release said %q", out.String())
	}

	// Freeing the holder's slot (a removed worktree, or hydra slots free)
	// gives it back.
	takeTunnel(wco, st, "tunnel", &out)
	r, err := reap(co, e, reaping{anyway: true})
	if err != nil || !strings.Contains(strings.Join(r.did, ","), "gave tunnel tunnel back to main") {
		t.Fatalf("reap = %+v, %v", r, err)
	}
	if held, _ := tunnels.Read(co.CommonDir); len(held) != 0 {
		t.Fatalf("still held after the reap: %+v", held)
	}

	// hydra down (and an idle stop) in the holder.
	takeTunnel(wco, st, "tunnel", &out)
	out.Reset()
	if names := releaseTunnelsOf(co.CommonDir, wt, "hydra down", &out); len(names) != 1 ||
		out.String() != "hydra: tunnel tunnel goes to main again (hooks-wt held it)\n" {
		t.Errorf("releaseTunnelsOf = %v, said %q", names, out.String())
	}
}

func TestTunnelName(t *testing.T) {
	st, _ := config.Parse([]byte(tunnelStack + `  tunnel2:
    command: x
    x-hydra: { tunnel: { to: api, port: 7080 } }
`))
	if _, err := tunnelName(st, nil); err == nil || !strings.Contains(err.Error(), "there are 2 tunnels; name one: tunnel, tunnel2") {
		t.Errorf("no name of two: %v", err)
	}
	if _, err := tunnelName(st, []string{"api"}); err == nil || !strings.Contains(err.Error(), `no tunnel is called "api"`) {
		t.Errorf("not a tunnel: %v", err)
	}
	if n, err := tunnelName(st, []string{"tunnel2"}); n != "tunnel2" || err != nil {
		t.Errorf("named = %q, %v", n, err)
	}
}

// What hydra status says: an error in the holder whose api is down (or
// whose stack is), a note in a worktree that runs the api and doesn't hold
// the tunnel, nothing otherwise.
func TestTunnelProblems(t *testing.T) {
	st, _ := config.Parse([]byte(tunnelStack))
	mainT := &target{stack: st}
	mainT.checkout.Root = "/repo/main"
	ms := &mainState{target: mainT}
	problems := func(holder, root string, runsAPI, up bool, api pc.Process) []problem {
		files := []routes.File{{Slot: 1, Root: "/repo/a", Local: map[string]int{"api": 7110}}, {Slot: 2, Root: "/repo/b", Local: map[string]int{}}}
		held := map[string]tunnels.Holder{}
		if holder != "" {
			held["tunnel"] = tunnels.Holder{Root: holder}
		}
		ms.tun = &tunnelCache{views: tunnelViews(st, held, files, func(routes.File) bool { return up }), at: time.Now()}
		wtT := &target{stack: st}
		wtT.checkout.Root = root
		p := &plan.Plan{Services: []plan.Service{{Name: "api", Local: runsAPI}}}
		ws := &wtState{target: wtT, Plan: p, Up: up, Procs: map[string]pc.Process{"api": api}}
		return tunnelProblems(ms, ws)
	}
	running, failed := pc.Process{Status: "Running", IsRunning: true}, pc.Process{Status: "Completed", ExitCode: 1}
	if ps := problems("/repo/a", "/repo/a", true, true, running); len(ps) != 0 {
		t.Errorf("holder, api up: %+v", ps)
	}
	if ps := problems("/repo/a", "/repo/a", true, true, failed); len(ps) != 1 || !ps[0].Blocks ||
		!strings.Contains(ps[0].Text, "holds tunnel tunnel (https://hooks.example.com), and its api is") || ps[0].Fix != "hydra up -D api   (or give the tunnel back: hydra tunnel release)" {
		t.Errorf("holder, api down: %+v", ps)
	}
	if ps := problems("/repo/a", "/repo/a", true, false, running); len(ps) != 1 || !ps[0].Blocks || !strings.Contains(ps[0].Text, "its stack isn't running") {
		t.Errorf("holder, stack down: %+v", ps)
	}
	if ps := problems("/repo/a", "/repo/b", true, true, running); len(ps) != 1 || ps[0].Blocks ||
		ps[0].Text != "webhooks through tunnel tunnel (https://hooks.example.com) go to a's api, not this worktree's." || !strings.HasPrefix(ps[0].Fix, "hydra tunnel take") {
		t.Errorf("another worktree holds it: %+v", ps)
	}
	if ps := problems("", "/repo/b", true, true, running); len(ps) != 1 || !strings.Contains(ps[0].Text, "go to main's api") {
		t.Errorf("main holds it: %+v", ps)
	}
	if ps := problems("", "/repo/b", false, true, running); len(ps) != 0 {
		t.Errorf("runs no api: %+v", ps)
	}
}
