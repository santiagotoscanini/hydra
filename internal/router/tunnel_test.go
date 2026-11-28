package router

import (
	"net/http"
	"strings"
	"testing"
)

// A tunnel's port follows the state: main's api, then the holder's own copy
// with its tag, then main's copy with the holder's tag, and a 502 that says
// why when the holder's copy (or its whole stack) is down. Never main's
// instead.
func TestTunnelGoesToWhoHoldsIt(t *testing.T) {
	g := newRig(t)
	port := freePort(t)
	mainAPI := g.st.Services["api"].Port
	set := func(tn Tunnel) {
		tn.Listen, tn.To = port, "api"
		g.mu.Lock()
		g.st.Tunnels = map[string]Tunnel{"hooks": tn}
		g.mu.Unlock()
		g.r.Reconcile()
	}
	// A caller can't pick the worktree with a tag of its own.
	call := func() (int, string, http.Header) { t.Helper(); return get(t, g.url(port)+"hook", TagHeader, "2") }

	set(Tunnel{Target: mainAPI, Label: "api@main"})
	if code, body, h := call(); code != 200 || body != `api@main saw ""` || h.Get(ServedByHeader) != "api@main" {
		t.Errorf("main holds it: %d %q (%s)", code, body, h.Get(ServedByHeader))
	}

	one := g.st.Worktrees["1"].Local["api"]
	set(Tunnel{Target: one, Label: "api@one", Tag: "1", Holder: "one", Own: true})
	if code, body, _ := call(); code != 200 || body != `api@one saw "1"` {
		t.Errorf("worktree one holds it: %d %q", code, body)
	}
	// The check is answered by the router, not passed on to the api.
	if code, body, h := get(t, g.url(port)+strings.TrimPrefix(TunnelCheckPath, "/")); code != 200 ||
		h.Get(ServedByHeader) != "api@one" || !strings.Contains(body, "hydra-router: tunnel hooks reaches api@one") {
		t.Errorf("the tunnel check: %d %q (%s)", code, body, h.Get(ServedByHeader))
	}

	set(Tunnel{Target: mainAPI, Label: "api@main for two", Tag: "2", Holder: "two"})
	if code, body, _ := call(); code != 200 || body != `api@main saw "2"` {
		t.Errorf("worktree two, which runs no api, holds it: %d %q", code, body)
	}

	down := g.st.Worktrees["3"].Local["api"]
	set(Tunnel{Target: down, Label: "api@three", Tag: "3", Holder: "three", Own: true})
	if code, body, _ := call(); code != http.StatusBadGateway || !strings.Contains(body, "held by worktree three, and its api isn't answering") ||
		!strings.Contains(body, "hydra tunnel release hooks") {
		t.Errorf("the holder's api is down: %d %q", code, body)
	}

	set(Tunnel{Target: down, Holder: "three", Down: "hydra: worktree three holds tunnel hooks, and its stack isn't running"})
	if code, body, _ := call(); code != http.StatusBadGateway || !strings.Contains(body, "its stack isn't running") {
		t.Errorf("the holder's stack is down: %d %q", code, body)
	}

	g.mu.Lock()
	active := strings.Join(g.active, ",")
	g.mu.Unlock()
	if !strings.Contains(active, "one") || !strings.Contains(active, "two") {
		t.Errorf("a webhook for a worktree is activity there: %q", active)
	}
}
