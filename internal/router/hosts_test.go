package router

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// named answers with its name and the Host and tag it saw, redirects to its
// own address on /redirect, and on an Upgrade: websocket request switches
// protocols and echoes what it is sent, as a dev server's hot-reload socket.
func named(name string) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			c, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer c.Close()
			fmt.Fprintf(buf, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nX-Name: %s\r\n\r\n", name)
			buf.Flush()
			line, _ := buf.ReadString('\n')
			fmt.Fprintf(c, "%s echoes %s", name, line)
			return
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, srv.URL+"/landing", http.StatusFound)
			return
		}
		fmt.Fprintf(w, "%s host=%s tag=%q", name, r.Host, r.Header.Get(TagHeader))
	}))
	return srv
}

type hostsRig struct {
	*rig
	hosts, apiHosts int
}

// newHostsRig is newRig with a hosts port for web and for api. Worktree
// "one" runs its own api and takes web through its routed entry; "two"
// runs a web of its own; "three" runs nothing; "four" runs a web that
// isn't up.
func newHostsRig(t *testing.T) *hostsRig {
	g := &hostsRig{rig: newRig(t)}
	wtWeb := named("web@two")
	t.Cleanup(wtWeb.Close)
	g.hosts = freePort(t)
	apiHosts := freePort(t)
	g.mu.Lock()
	web := g.st.Services["web"]
	web.Hosts = g.hosts
	g.st.Services["web"] = web
	api := g.st.Services["api"]
	api.Hosts = apiHosts
	g.st.Services["api"] = api
	one := g.st.Worktrees["1"]
	one.Host = "one"
	g.st.Worktrees["1"] = one
	g.st.Worktrees["2"] = Worktree{Name: "two", Host: "two", Local: map[string]int{"web": portOf(t, wtWeb)}}
	g.st.Worktrees["4"] = Worktree{Name: "four", Host: "four", Local: map[string]int{"web": freePort(t)}}
	three := g.st.Worktrees["3"]
	three.Host, three.Local, three.Entries = "three", nil, nil
	g.st.Worktrees["3"] = three
	g.mu.Unlock()
	g.r.Reconcile()
	g.apiHosts = apiHosts
	return g
}

func hostGet(t *testing.T, port int, host, path string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(port)+path, nil)
	req.Host = host
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatalf("GET %s on :%d: %v", host, port, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(body)), resp.Header
}

func TestHostsPortServesEachWorktreeByName(t *testing.T) {
	g := newHostsRig(t)
	at := func(host string) string { return host + ":" + strconv.Itoa(g.hosts) }

	// Plain localhost, and main.localhost, are main's copy.
	for _, h := range []string{at("localhost"), at("main.localhost"), at("127.0.0.1")} {
		if code, body, hd := hostGet(t, g.hosts, h, "/"); code != 200 || body != `web -> api@main saw ""` || hd.Get(ServedByHeader) != "web@main" {
			t.Errorf("%s: %d %q (%s)", h, code, body, hd.Get(ServedByHeader))
		}
	}
	// A worktree that takes the routed web from main gets it the way its
	// own port does: tagged, so web's calls reach the worktree's api.
	if code, body, _ := hostGet(t, g.hosts, at("one.localhost"), "/"); code != 200 || body != `web -> api@one saw "1"` {
		t.Errorf("one: %d %q", code, body)
	}
	// A worktree with a web of its own gets that, with the Host it was
	// asked for.
	if code, body, hd := hostGet(t, g.hosts, at("TWO.localhost"), "/"); code != 200 ||
		body != `web@two host=`+at("TWO.localhost")+` tag=""` || hd.Get(ServedByHeader) != "web@two" {
		t.Errorf("two: %d %q (%s)", code, body, hd.Get(ServedByHeader))
	}
	// A worktree that runs no copy and has no entry uses main's, untagged.
	if code, body, hd := hostGet(t, g.hosts, at("three.localhost"), "/"); code != 200 || body != `web -> api@main saw ""` ||
		hd.Get(ServedByHeader) != "web@main for three" {
		t.Errorf("three: %d %q (%s)", code, body, hd.Get(ServedByHeader))
	}
	// One whose copy isn't up never gets main's instead.
	if code, body, _ := hostGet(t, g.hosts, at("four.localhost"), "/"); code != 503 || !strings.Contains(body, "web in worktree four isn't accepting") {
		t.Errorf("four: %d %q", code, body)
	}
	// An unknown name says which ones run.
	if code, body, _ := hostGet(t, g.hosts, at("nope.localhost"), "/"); code != 404 || !strings.Contains(body, `no running worktree is called "nope" (running: four, one, three, two)`) {
		t.Errorf("unknown: %d %q", code, body)
	}
	// Another host altogether isn't guessed at.
	if code, body, _ := hostGet(t, g.hosts, at("example.com"), "/"); code != 404 || !strings.Contains(body, "http://<worktree>.localhost:"+strconv.Itoa(g.hosts)) {
		t.Errorf("other host: %d %q", code, body)
	}
	// The api's hosts port: worktree one's own copy.
	if _, body, _ := hostGet(t, g.apiHosts, "one.localhost", "/"); body != `api@one saw ""` {
		t.Errorf("api one: %q", body)
	}
	// Using an address is using that worktree's stack.
	g.mu.Lock()
	active := strings.Join(g.active, ",")
	g.mu.Unlock()
	for _, name := range []string{"one", "two", "three"} {
		if !strings.Contains(active, name) {
			t.Errorf("activity %q lacks %s", active, name)
		}
	}
}

func TestHostsPortRefusesTwoWorktreesOfOneName(t *testing.T) {
	g := newHostsRig(t)
	g.mu.Lock()
	g.st.Worktrees["5"] = Worktree{Name: "other", Host: "two", Root: "/elsewhere/two"}
	g.mu.Unlock()
	g.r.Reconcile()
	if code, body, _ := hostGet(t, g.hosts, "two.localhost", "/"); code != 409 || !strings.Contains(body, "/elsewhere/two") {
		t.Errorf("%d %q", code, body)
	}
}

func TestHostsPortKeepsARedirectOnTheName(t *testing.T) {
	g := newHostsRig(t)
	_, _, h := hostGet(t, g.hosts, "two.localhost:"+strconv.Itoa(g.hosts), "/redirect")
	if want := "http://two.localhost:" + strconv.Itoa(g.hosts) + "/landing"; h.Get("Location") != want {
		t.Errorf("Location = %q, want %q", h.Get("Location"), want)
	}
}

// The risky part: a dev server's hot reload is a websocket, so the upgrade
// has to pass through to the worktree's copy, and bytes flow both ways after.
func TestHostsPortPassesAWebsocketUpgrade(t *testing.T) {
	g := newHostsRig(t)
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(g.hosts), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "GET /_next/webpack-hmr HTTP/1.1\r\nHost: two.localhost:%d\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", g.hosts)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 101 || resp.Header.Get("X-Name") != "web@two" {
		t.Fatalf("upgrade: %d from %q", resp.StatusCode, resp.Header.Get("X-Name"))
	}
	fmt.Fprint(c, "ping\n")
	got, _ := io.ReadAll(br)
	if string(got) != "web@two echoes ping\n" {
		t.Errorf("after the upgrade: %q", got)
	}
}

func TestHostLabel(t *testing.T) {
	for in, want := range map[string]string{
		"localhost:3100": MainHost, "[::1]:3100": MainHost, "fla-709.localhost:3100": "fla-709",
		"FLA-709.LOCALHOST.": "fla-709", "a.b.localhost": "a.b", ".localhost": "", "example.com:3100": "",
	} {
		if got := hostLabel(in); got != want {
			t.Errorf("hostLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
