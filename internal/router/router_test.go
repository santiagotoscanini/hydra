package router

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func portOf(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	_, p, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	n, _ := strconv.Atoi(p)
	return n
}

// api answers with its own name and the tag it saw.
func api(name string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s saw %q", name, r.Header.Get(TagHeader))
	}))
}

// web is a routed service: it calls the api through the router and, when
// forward is set, passes the tag of the request it is serving along. It also
// answers the probe the way hydra's hook does.
func web(apiURL func() string, forward *bool) *httptest.Server {
	call := func(tag, path string) (*http.Response, error) {
		req, _ := http.NewRequest("GET", apiURL()+path, nil)
		if *forward && tag != "" {
			req.Header.Set(TagHeader, tag)
		}
		return http.DefaultClient.Do(req)
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tag := r.Header.Get(TagHeader)
		switch r.URL.Path {
		case "/redirect-home": // an absolute URL built from its own address
			http.Redirect(w, r, srv.URL+"/landing?x=1", http.StatusFound)
			return
		case "/redirect-away":
			http.Redirect(w, r, "https://example.com/x", http.StatusFound)
			return
		}
		if r.URL.Path == probePath {
			resp, err := call(tag, echoPath)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			defer resp.Body.Close()
			var echo struct{ Tag string }
			json.NewDecoder(resp.Body).Decode(&echo)
			json.NewEncoder(w).Encode(map[string]any{"hook": true, "calls": []map[string]string{{"origin": apiURL(), "tag": echo.Tag}}})
			return
		}
		resp, err := call(tag, "/")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		w.Header().Set("X-Api-Served-By", resp.Header.Get(ServedByHeader))
		w.Header().Set("X-Saw-Upstreams", r.Header.Get(UpstreamsHeader))
		fmt.Fprintf(w, "web -> %s", body)
	}))
	return srv
}

func get(t *testing.T, url string, headers ...string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(body)), resp.Header
}

type rig struct {
	t                    *testing.T
	r                    *Router
	mu                   sync.Mutex
	st                   State
	dispatch, e1, e2, e3 int
	forward              bool
	logs                 []string
}

// newRig: main runs web and api. Worktree 1 runs its own api, worktree 2
// runs nothing of its own, worktree 3 says it runs an api that isn't up yet.
func newRig(t *testing.T) *rig {
	g := &rig{t: t, forward: true}
	g.dispatch, g.e1, g.e2, g.e3 = freePort(t), freePort(t), freePort(t), freePort(t)
	mainAPI, wtAPI := api("api@main"), api("api@one")
	mainWeb := web(func() string { return "http://127.0.0.1:" + strconv.Itoa(g.dispatch) }, &g.forward)
	t.Cleanup(func() { mainAPI.Close(); wtAPI.Close(); mainWeb.Close() })

	g.st = State{
		Host: "127.0.0.1",
		Services: map[string]Service{
			"web": {Port: portOf(t, mainWeb), Probe: true},
			"api": {Port: portOf(t, mainAPI), Dispatch: g.dispatch},
		},
		Worktrees: map[string]Worktree{
			"1": {Name: "one", Local: map[string]int{"api": portOf(t, wtAPI)},
				Entries: map[string]Entry{"web": {Port: g.e1, Upstreams: map[string]string{"QUEUE": "hydra 1"}}}},
			"2": {Name: "two", Entries: map[string]Entry{"web": {Port: g.e2}}},
			"3": {Name: "three", Local: map[string]int{"api": freePort(t)}, Entries: map[string]Entry{"web": {Port: g.e3}}},
		},
	}
	g.r = &Router{
		Load: func() (*State, error) { g.mu.Lock(); defer g.mu.Unlock(); st := g.st; return &st, nil },
		Logf: func(f string, a ...any) { g.mu.Lock(); g.logs = append(g.logs, fmt.Sprintf(f, a...)); g.mu.Unlock() },
	}
	t.Cleanup(g.r.Close)
	g.r.Reconcile()
	g.waitProbed("web")
	return g
}

func (g *rig) waitProbed(name string) {
	g.t.Helper()
	for i := 0; i < 200; i++ {
		if h, ok := g.r.health.Load(name); ok && h.(health).checked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	g.t.Fatalf("%s was never checked", name)
}

func (g *rig) url(port int) string { return "http://127.0.0.1:" + strconv.Itoa(port) + "/" }

func TestRoutesEachWorktreeToItsOwnCopy(t *testing.T) {
	g := newRig(t)

	// Worktree 1 runs an api: main's web, reached through its port, calls it.
	code, body, h := get(t, g.url(g.e1))
	if code != 200 || body != `web -> api@one saw "1"` {
		t.Errorf("worktree 1: %d %q", code, body)
	}
	if got := h.Get("X-Api-Served-By"); got != "api@one" {
		t.Errorf("served-by = %q", got)
	}
	if got := h.Get("X-Saw-Upstreams"); got != "QUEUE=hydra%201" {
		t.Errorf("per-request values = %q", got)
	}

	// Worktree 2 runs none: the same web calls main's api, still tagged.
	if _, body, _ := get(t, g.url(g.e2)); body != `web -> api@main saw "2"` {
		t.Errorf("worktree 2: %q", body)
	}

	// A tag sent by the client is replaced: the port decides.
	if _, body, _ := get(t, g.url(g.e2), TagHeader, "1"); body != `web -> api@main saw "2"` {
		t.Errorf("spoofed tag: %q", body)
	}

	// An untagged call through the router is main's.
	if _, body, h := get(t, g.url(g.dispatch)); body != `api@main saw ""` || h.Get(ServedByHeader) != "api@main" {
		t.Errorf("untagged: %q, served by %q", body, h.Get(ServedByHeader))
	}

	// A tag no worktree has falls to main's and says so.
	if _, _, h := get(t, g.url(g.dispatch), TagHeader, "9"); !strings.Contains(h.Get(ServedByHeader), "no running worktree") {
		t.Errorf("unknown tag: served by %q", h.Get(ServedByHeader))
	}
}

func TestNeverFallsBackToMainForAWorktreeThatRunsItsOwn(t *testing.T) {
	g := newRig(t)
	// Worktree 3's api isn't listening yet (still compiling, say).
	code, body, _ := get(t, g.url(g.dispatch), TagHeader, "3")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "api in worktree three isn't accepting connections") {
		t.Errorf("got %d %q", code, body)
	}
}

func TestRefusesAServiceThatDropsTheTag(t *testing.T) {
	g := newRig(t)
	g.forward = false // web stops passing the tag on
	g.r.mu.Lock()
	g.r.probed = map[string]time.Time{}
	g.r.mu.Unlock()
	g.r.health.Delete("web")
	g.r.Reconcile()
	g.waitProbed("web")

	code, body, _ := get(t, g.url(g.e1))
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "arrived without the worktree tag") || !strings.Contains(body, "hydra up web") {
		t.Errorf("got %d %q", code, body)
	}
}

func TestListenersFollowTheState(t *testing.T) {
	g := newRig(t)
	g.mu.Lock()
	delete(g.st.Worktrees, "2") // the worktree went down
	g.mu.Unlock()
	g.r.Reconcile()
	if _, err := http.Get(g.url(g.e2)); err == nil {
		t.Error("worktree 2's port still answers after it went down")
	}
	if code, _, _ := get(t, g.url(g.e1)); code != 200 {
		t.Errorf("worktree 1 broke: %d", code)
	}
}

func TestRefusesWhenTheTemporalSwitchIsBroken(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"hook":true,"temporal":"broken: the Temporal client kept namespace \"default\"","calls":[]}`)
	}))
	defer web.Close()
	r := &Router{client: &http.Client{Timeout: time.Second}}
	st := &State{Host: "127.0.0.1", Services: map[string]Service{"web": {Port: portOf(t, web), Probe: true}}}
	reason, running := r.check("web", st, "probe-1")
	if !running || !strings.Contains(reason, "can't switch its Temporal namespace") {
		t.Errorf("reason = %q, running = %v", reason, running)
	}
}

func TestARedirectToMainsAddressStaysOnTheWorktreesPort(t *testing.T) {
	g := newRig(t)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	location := func(path string) string {
		t.Helper()
		resp, err := noFollow.Get(strings.TrimSuffix(g.url(g.e1), "/") + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("Location")
	}
	// main's web redirects to its own address: followed as is, the browser
	// would leave worktree 1's port and its next requests would be main's.
	if got, want := location("/redirect-home"), fmt.Sprintf("http://127.0.0.1:%d/landing?x=1", g.e1); got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if got := location("/redirect-away"); got != "https://example.com/x" {
		t.Errorf("a redirect elsewhere was rewritten: %q", got)
	}
}

func TestStatusSaysWhetherMainsCopyCanServe(t *testing.T) {
	g := newRig(t)
	status := func() (out struct {
		Router   bool
		Service  string
		Worktree string
		Reason   string
	}) {
		t.Helper()
		_, body, _ := get(t, strings.TrimSuffix(g.url(g.e1), "/")+StatusPath)
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("status: %v in %q", err, body)
		}
		return out
	}
	if s := status(); !s.Router || s.Service != "web" || s.Worktree != "1" || s.Reason != "" {
		t.Errorf("status = %+v", s)
	}
	g.forward = false // web stops passing the tag on
	g.r.mu.Lock()
	g.r.probed = map[string]time.Time{}
	g.r.mu.Unlock()
	g.r.health.Delete("web")
	g.r.Reconcile()
	g.waitProbed("web")
	if s := status(); !strings.Contains(s.Reason, "without the worktree tag") {
		t.Errorf("status after the tag is dropped = %+v", s)
	}
}
