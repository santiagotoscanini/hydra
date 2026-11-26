package router

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// MainHost is the name main's copy answers to on a hosts port, besides
// plain localhost.
const MainHost = "main"

// hostTarget is where a request on a hosts port goes.
type hostTarget struct {
	port     int    // a copy to pass it to; 0 with entry set
	label    string // "web@fla-709", "web@main"
	worktree string // its name; "" = main's
	tag      string // the worktree's tag, for the routed entry and Activity
	entry    bool   // a routed service the worktree takes from main: through its entry
	own      bool   // the worktree's own copy
}

// hostLabel is the worktree name a Host header asks for: "fla-709" for
// "fla-709.localhost:3100", MainHost for main's own names, "" for a host
// that isn't a name under localhost at all.
func hostLabel(hostport string) string {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	switch h {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return MainHost
	}
	label, ok := strings.CutSuffix(h, ".localhost")
	if !ok || label == "" {
		return ""
	}
	return label
}

// resolveHost picks the copy of a service a request on its hosts port is
// for, by the name in its Host header: main's copy for localhost, and for
// <worktree>.localhost whatever that worktree uses as the service, the way
// its own wiring would reach it. The error is the reason it can't be served,
// with the HTTP status to answer.
func (st *State) resolveHost(name, hostport string) (hostTarget, int, string) {
	base := hostTarget{port: st.Services[name].Port, label: name + "@main"}
	label := hostLabel(hostport)
	switch label {
	case "":
		return hostTarget{}, http.StatusNotFound, "this port serves " + name + " by worktree name: open http://<worktree>.localhost:" +
			hostPort(hostport) + " (or localhost for main's)"
	case MainHost:
		return base, 0, ""
	}
	var tags []string
	for tag, wt := range st.Worktrees {
		if wt.Host == label {
			tags = append(tags, tag)
		}
	}
	sort.Strings(tags)
	switch len(tags) {
	case 0:
		return hostTarget{}, http.StatusNotFound, "no running worktree is called " + strconv.Quote(label) + st.runningNames() +
			". A worktree's address works while its stack runs: `hydra up` in it"
	case 1:
	default:
		var roots []string
		for _, tag := range tags {
			roots = append(roots, st.Worktrees[tag].Root)
		}
		return hostTarget{}, http.StatusConflict, "more than one running worktree has a folder called " + strconv.Quote(label) +
			" (" + strings.Join(roots, ", ") + "); rename one of the folders to tell them apart"
	}
	tag := tags[0]
	wt := st.Worktrees[tag]
	if port, ok := wt.Local[name]; ok {
		return hostTarget{port: port, label: name + "@" + wt.Name, worktree: wt.Name, tag: tag, own: true}, 0, ""
	}
	if _, ok := wt.Entries[name]; ok {
		return hostTarget{label: name + "@main for " + wt.Name, worktree: wt.Name, tag: tag, entry: true}, 0, ""
	}
	// The worktree takes it from main as it is: main's copy, untagged, as
	// the worktree's own services reach it.
	base.label = name + "@main for " + wt.Name
	base.tag = tag
	return base, 0, ""
}

func (st *State) runningNames() string {
	var names []string
	for _, wt := range st.Worktrees {
		if wt.Host != "" {
			names = append(names, wt.Host)
		}
	}
	if len(names) == 0 {
		return " (none runs now)"
	}
	sort.Strings(names)
	return " (running: " + strings.Join(names, ", ") + ")"
}

func hostPort(hostport string) string {
	if _, p, err := net.SplitHostPort(hostport); err == nil {
		return p
	}
	return "PORT"
}

type hostKey struct{}

// hosts handles a service's hosts port: one address per worktree, by name
// (see State.resolveHost). The Host header is passed on as it came, so the
// service builds its links and checks its origin against the address the
// browser is on, and a websocket upgrade (a dev server's hot reload) goes
// through like any other request.
func (r *Router) hosts(name string) http.Handler {
	proxy := &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			t := pr.In.Context().Value(hostKey{}).(hostTarget)
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = net.JoinHostPort(r.state.Load().Host, strconv.Itoa(t.port))
			pr.Out.Host = pr.In.Host
			stripHydra(pr.Out.Header)
		},
		ModifyResponse: func(resp *http.Response) error {
			t := resp.Request.Context().Value(hostKey{}).(hostTarget)
			resp.Header.Set(ServedByHeader, t.label)
			// A redirect to the copy's own port would take the browser off
			// the name address.
			if loc, err := url.Parse(resp.Header.Get("Location")); err == nil && loc.Host != "" &&
				isLoopbackPort(loc.Host, r.state.Load().Host, t.port) {
				loc.Host = resp.Request.Host
				resp.Header.Set("Location", loc.String())
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			t := req.Context().Value(hostKey{}).(hostTarget)
			if t.own {
				// Never main's instead: the page would be main's while the
				// address says it is the branch's.
				fail(w, http.StatusServiceUnavailable, "%s in worktree %s isn't accepting connections on :%d yet (%v)", name, t.worktree, t.port, err)
				return
			}
			fail(w, http.StatusBadGateway, "main's %s isn't answering on :%d (%v). Is main's stack running?", name, t.port, err)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		st := r.state.Load()
		if req.URL.Path == StatusPath {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"router": true, "service": name, "hosts": true})
			return
		}
		t, status, why := st.resolveHost(name, req.Host)
		if status != 0 {
			fail(w, status, "%s", why)
			return
		}
		if t.entry {
			// A routed service: exactly what the worktree's own port for it
			// does (the tag, its per-request values, the health check).
			r.entry(t.tag, name).ServeHTTP(w, req)
			return
		}
		r.active(t.tag)
		proxy.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), hostKey{}, t)))
	})
}
