// Package router lets one copy of a service serve several worktrees.
//
// It runs in the baseline and listens on two kinds of port:
//
//   - A worktree's port for a routed service it didn't change (web on :3400
//     for slot 2). A request there gets the worktree's tag and goes to the
//     baseline's copy.
//   - One port per service that routed services call (the api). A call there
//     goes to the tagged worktree's copy of that service if the worktree
//     runs one, and to the baseline's otherwise.
//   - A TLS front for a baseline service that asks for one: TLS is accepted
//     there and the bytes go on, as they are, to the service's plain port.
//
// In between, the routed service has to copy the tag from the request it is
// serving onto the calls it makes. The router checks that it does before it
// lets a worktree use it.
package router

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// TagHeader names the worktree a request is for.
	TagHeader = "X-Hydra-Worktree"
	// UpstreamsHeader carries the values a routed service picks per request
	// itself: "ENV=value;ENV2=value2", values URL-escaped.
	UpstreamsHeader = "X-Hydra-Upstreams"
	// ServedByHeader, on a response, says which copy answered.
	ServedByHeader = "X-Hydra-Served-By"

	probePath = "/__hydra/probe"
	echoPath  = "/__hydra/echo"
	// StatusPath, on a worktree's port for a routed service, is answered by
	// the router itself: whether main's copy can serve that worktree.
	StatusPath = "/__hydra/status"
)

// Service is a baseline service the router knows.
type Service struct {
	Port     int
	Dispatch int  // port for calls to it from routed services; 0 = none
	Probe    bool // routed with hydra's hook: check that it passes the tag on
}

// Entry is a routed service behind a worktree's port.
type Entry struct {
	Port      int
	Upstreams map[string]string
}

// Worktree is one running worktree.
type Worktree struct {
	Name    string
	Local   map[string]int   // service -> port of the copy it runs
	Entries map[string]Entry // routed service -> its port in this worktree
}

// Front is a TLS front: TLS is accepted on Listen and the bytes are passed
// to Target, the baseline service's plain port. ALPN is the protocol the
// front offers, which has to be the one the service speaks in the clear:
// "http/1.1" or "h2".
type Front struct {
	Listen int
	Target int
	ALPN   string
}

// State is everything the router routes by at one moment.
type State struct {
	Host      string
	Services  map[string]Service
	Worktrees map[string]Worktree // by tag
	// Fronts are the TLS fronts, by service, and Cert what they present. A
	// new Cert is used from the next handshake on.
	Fronts map[string]Front
	Cert   *tls.Certificate
}

// Router keeps its listeners in step with the state Load returns.
type Router struct {
	Load     func() (*State, error)
	Logf     func(format string, args ...any)
	Interval time.Duration // between reloads; default 500ms
	ProbeGap time.Duration // between checks of a routed service; default 10s

	state     atomic.Pointer[State]
	mu        sync.Mutex
	listeners map[string]*listener
	failed    map[string]string // listener key -> last listen error, to log once
	health    sync.Map          // service -> health
	probed    map[string]time.Time
	client    *http.Client
}

type listener struct {
	servers []*http.Server
	raw     []net.Listener // TLS fronts: closed as they are
	port    int
	what    string
}

// health is the result of the last check of a routed service.
type health struct {
	checked bool
	reason  string // why it can't serve worktrees; "" = fine
}

// Run reconciles until ctx ends, then closes every listener.
func (r *Router) Run(ctx context.Context) error {
	if r.Interval == 0 {
		r.Interval = 500 * time.Millisecond
	}
	if r.ProbeGap == 0 {
		r.ProbeGap = 10 * time.Second
	}
	defer r.Close()
	for {
		r.Reconcile()
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.Interval):
		}
	}
}

// Close stops every listener.
func (r *Router) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, l := range r.listeners {
		l.close()
		delete(r.listeners, key)
	}
}

// Reconcile reloads the state and opens or closes listeners to match it.
func (r *Router) Reconcile() {
	st, err := r.Load()
	if err != nil {
		r.logf("can't read the routes: %v", err)
		return
	}
	r.state.Store(st)

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listeners == nil {
		r.listeners, r.failed, r.probed = map[string]*listener{}, map[string]string{}, map[string]time.Time{}
		r.client = &http.Client{Timeout: 5 * time.Second}
	}

	// Each wanted listener: its port, what it is for, and how to open it.
	type opener struct {
		port int
		what string
		open func() (*listener, error)
	}
	want := map[string]opener{}
	for name, s := range st.Services {
		if s.Dispatch != 0 {
			name, port := name, s.Dispatch
			want[fmt.Sprintf("dispatch %s :%d", name, port)] = opener{port, fmt.Sprintf("calls to %s from routed services", name),
				func() (*listener, error) { return listen(st.Host, port, r.dispatch(name)) }}
		}
	}
	for tag, wt := range st.Worktrees {
		for name, e := range wt.Entries {
			tag, name, port := tag, name, e.Port
			want[fmt.Sprintf("entry %s %s :%d", tag, name, port)] = opener{port, fmt.Sprintf("%s for worktree %s", name, wt.Name),
				func() (*listener, error) { return listen(st.Host, port, r.entry(tag, name)) }}
		}
	}
	if st.Cert != nil {
		for name, f := range st.Fronts {
			f := f
			want[fmt.Sprintf("tls %s :%d>%d %s", name, f.Listen, f.Target, f.ALPN)] = opener{f.Listen,
				fmt.Sprintf("TLS for %s (:%d, %s)", name, f.Target, f.ALPN),
				func() (*listener, error) { return r.listenTLS(st.Host, f) }}
		}
	}

	for key, l := range r.listeners {
		if _, wanted := want[key]; !wanted {
			l.close()
			delete(r.listeners, key)
			r.logf("closed :%d: %s", l.port, l.what)
		}
	}
	keys := make([]string, 0, len(want))
	for key := range want {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if r.listeners[key] != nil {
			continue
		}
		port, what := want[key].port, want[key].what
		l, err := want[key].open()
		if err != nil {
			if r.failed[key] != err.Error() { // say it once; the port may free up
				r.failed[key] = err.Error()
				r.logf("can't listen on :%d (%s): %v; retrying", port, what, err)
			}
			continue
		}
		delete(r.failed, key)
		l.port, l.what = port, what
		r.listeners[key] = l
		r.logf("listening on :%d: %s", port, what)
	}

	for name, s := range st.Services {
		if s.Probe && time.Since(r.probed[name]) >= r.ProbeGap {
			r.probed[name] = time.Now()
			go r.probe(name, st)
		}
	}
}

// listen serves handler on host:port. For a loopback host it also takes the
// other loopback family when it can: a browser may resolve localhost to ::1.
func listen(host string, port int, handler http.Handler) (*listener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	l := &listener{}
	serve := func(ln net.Listener) {
		srv := &http.Server{Handler: handler}
		l.servers = append(l.servers, srv)
		go srv.Serve(ln)
	}
	serve(ln)
	if other := map[string]string{"127.0.0.1": "::1", "::1": "127.0.0.1"}[host]; other != "" {
		if ln2, err := net.Listen("tcp", net.JoinHostPort(other, strconv.Itoa(port))); err == nil {
			serve(ln2)
		}
	}
	return l, nil
}

func (l *listener) close() {
	for _, srv := range l.servers {
		srv.Close()
	}
	for _, ln := range l.raw {
		ln.Close()
	}
}

// listenTLS opens a TLS front. It doesn't look inside the connection: after
// the handshake the bytes go to the service as they came, so it carries
// whatever the service speaks in the clear (HTTP/1.1 with its websockets, or
// HTTP/2 for a gRPC server), as long as ALPN names it. The certificate is
// read from the state at every handshake, so a renewed one needs no restart.
func (r *Router) listenTLS(host string, f Front) (*listener, error) {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{f.ALPN},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			if st := r.state.Load(); st != nil && st.Cert != nil {
				return st.Cert, nil
			}
			return nil, errors.New("hydra has no certificate for its TLS fronts")
		},
	}
	target := net.JoinHostPort(host, strconv.Itoa(f.Target))
	ln, err := tls.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(f.Listen)), cfg)
	if err != nil {
		return nil, err
	}
	l := &listener{raw: []net.Listener{ln}}
	go forward(ln, target)
	// As for the other ports: a client may resolve localhost to the other
	// loopback family.
	if other := map[string]string{"127.0.0.1": "::1", "::1": "127.0.0.1"}[host]; other != "" {
		if ln2, err := tls.Listen("tcp", net.JoinHostPort(other, strconv.Itoa(f.Listen)), cfg); err == nil {
			l.raw = append(l.raw, ln2)
			go forward(ln2, target)
		}
	}
	return l, nil
}

// forward passes every connection accepted on ln to target, until ln closes.
func forward(ln net.Listener, target string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			up, err := net.DialTimeout("tcp", target, 5*time.Second)
			if err != nil {
				return // the service is down: the client sees the connection close
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			pipe := func(dst, src net.Conn) {
				io.Copy(dst, src)
				// Pass the end of one direction on, and let the other finish.
				if cw, ok := dst.(interface{ CloseWrite() error }); ok {
					cw.CloseWrite()
				}
				done <- struct{}{}
			}
			go pipe(up, c)
			go pipe(c, up)
			<-done
			<-done
		}()
	}
}

func (r *Router) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// entry handles a worktree's port for a routed service: tag the request and
// pass it to the baseline's copy.
func (r *Router) entry(tag, name string) http.Handler {
	proxy := &httputil.ReverseProxy{
		FlushInterval: -1, // streams and server-sent events pass straight through
		Rewrite: func(pr *httputil.ProxyRequest) {
			st := r.state.Load()
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = net.JoinHostPort(st.Host, strconv.Itoa(st.Services[name].Port))
			// The Host header stays the worktree's, so redirects and
			// same-origin checks keep the browser on this port.
			pr.Out.Host = pr.In.Host
			stripHydra(pr.Out.Header) // only this port decides the tag
			pr.Out.Header.Set(TagHeader, tag)
			if ups := st.Worktrees[tag].Entries[name].Upstreams; len(ups) > 0 {
				pr.Out.Header.Set(UpstreamsHeader, encodeUpstreams(ups))
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			// A redirect to main's own address (built from an env var main's
			// copy read once) would take the browser off this worktree's
			// port, and its next requests would be main's.
			st := r.state.Load()
			if loc, err := url.Parse(resp.Header.Get("Location")); err == nil && loc.Host != "" &&
				isLoopbackPort(loc.Host, st.Host, st.Services[name].Port) {
				loc.Host = resp.Request.Host
				resp.Header.Set("Location", loc.String())
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			port := r.state.Load().Services[name].Port
			fail(w, http.StatusBadGateway, "main's %s isn't answering on :%d (%v). Is main's stack running?", name, port, err)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == StatusPath {
			h, _ := r.health.Load(name)
			hv, _ := h.(health)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"router": true, "service": name, "worktree": tag, "checked": hv.checked, "reason": hv.reason,
			})
			return
		}
		if h, ok := r.health.Load(name); ok && h.(health).reason != "" {
			fail(w, http.StatusServiceUnavailable,
				"main's %s can't serve this worktree: %s. Run `hydra up %s` here for a copy of your own.", name, h.(health).reason, name)
			return
		}
		proxy.ServeHTTP(w, req)
	})
}

type target struct {
	port     int
	label    string // "api@fix-cart", "api@main"
	worktree string // name of the worktree whose copy it is; "" = baseline
}

type targetKey struct{}

// resolve picks the copy of a service a tagged call goes to.
func (st *State) resolve(name, tag string) target {
	base := target{port: st.Services[name].Port, label: name + "@main"}
	if tag == "" {
		return base
	}
	wt, ok := st.Worktrees[tag]
	if !ok {
		base.label += " (no running worktree has tag " + strconv.Quote(tag) + ")"
		return base
	}
	if port, ok := wt.Local[name]; ok {
		return target{port: port, label: name + "@" + wt.Name, worktree: wt.Name}
	}
	return base
}

// dispatch handles calls to a service from routed services.
func (r *Router) dispatch(name string) http.Handler {
	proxy := &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			t := pr.In.Context().Value(targetKey{}).(target)
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = net.JoinHostPort(r.state.Load().Host, strconv.Itoa(t.port))
			pr.Out.Host = pr.Out.URL.Host
			pr.Out.Header.Del("X-Hydra-Hook")
		},
		ModifyResponse: func(resp *http.Response) error {
			t := resp.Request.Context().Value(targetKey{}).(target)
			resp.Header.Set(ServedByHeader, t.label)
			if tag := resp.Request.Header.Get(TagHeader); tag != "" {
				r.logf("%s %s -> %s %d", resp.Request.Method, resp.Request.URL.Path, t.label, resp.StatusCode)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			t := req.Context().Value(targetKey{}).(target)
			if t.worktree != "" {
				// Never fall back to main's copy here: the caller would test
				// main while believing it is testing the branch.
				fail(w, http.StatusServiceUnavailable, "%s in worktree %s isn't accepting connections on :%d yet (%v)", name, t.worktree, t.port, err)
				return
			}
			fail(w, http.StatusBadGateway, "main's %s isn't answering on :%d (%v)", name, t.port, err)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		tag := req.Header.Get(TagHeader)
		t := r.state.Load().resolve(name, tag)
		if req.URL.Path == echoPath {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"service": name, "tag": tag, "target": t.label})
			return
		}
		proxy.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), targetKey{}, t)))
	})
}

// probe checks that a routed service passes the tag on: it sends a request
// with a tag no worktree has and asks hydra's hook, inside the service, to
// call every routed address and report the tag each one saw.
func (r *Router) probe(name string, st *State) {
	tag := "probe-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	reason, running := r.check(name, st, tag)
	if !running {
		// Nothing to check yet (requests get a 502 from the proxy). Look
		// again on the next reload, so a copy that just started is checked
		// before a worktree uses it.
		r.mu.Lock()
		delete(r.probed, name)
		r.mu.Unlock()
		return
	}
	prev, _ := r.health.Load(name)
	r.health.Store(name, health{checked: true, reason: reason})
	if p, _ := prev.(health); p.checked && p.reason == reason {
		return
	}
	if reason == "" {
		r.logf("%s passes the worktree tag on: worktrees can use main's copy", name)
	} else {
		r.logf("%s can't serve worktrees: %s", name, reason)
	}
}

func (r *Router) check(name string, st *State, tag string) (reason string, running bool) {
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(st.Host, strconv.Itoa(st.Services[name].Port)), Path: probePath}
	req, _ := http.NewRequest("GET", u.String(), nil)
	req.Header.Set(TagHeader, tag)
	resp, err := r.client.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return "", false
		}
		return "the check failed: " + err.Error(), true
	}
	defer resp.Body.Close()
	var out struct {
		Hook     bool   `json:"hook"`
		Temporal string `json:"temporal"`
		Calls    []struct {
			Origin string `json:"origin"`
			Tag    string `json:"tag"`
			Error  string `json:"error"`
		} `json:"calls"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &out) != nil || !out.Hook {
		return "it isn't running hydra's hook (start main's stack with `hydra up` so it is loaded)", true
	}
	if t, ok := strings.CutPrefix(out.Temporal, "broken: "); ok {
		return "hydra can't switch its Temporal namespace per worktree (" + t + ")", true
	}
	for _, c := range out.Calls {
		switch {
		case c.Error != "":
			return fmt.Sprintf("its call to %s failed: %s", c.Origin, c.Error), true
		case c.Tag != tag:
			return fmt.Sprintf("its call to %s arrived without the worktree tag", c.Origin), true
		}
	}
	return "", true
}

// isLoopbackPort reports whether hostport is this machine on the given
// port, under any of the names a browser or an env var uses for it.
func isLoopbackPort(hostport, host string, port int) bool {
	h, p, err := net.SplitHostPort(hostport)
	if err != nil || p != strconv.Itoa(port) {
		return false
	}
	return h == host || h == "localhost" || h == "127.0.0.1" || h == "::1"
}

func stripHydra(h http.Header) {
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-hydra-") {
			h.Del(k)
		}
	}
}

func encodeUpstreams(ups map[string]string) string {
	keys := make([]string, 0, len(ups))
	for k := range ups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + url.PathEscape(ups[k])
	}
	return strings.Join(parts, ";")
}

func fail(w http.ResponseWriter, status int, format string, args ...any) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set(ServedByHeader, "hydra")
	w.WriteHeader(status)
	fmt.Fprintf(w, "hydra: "+format+"\n", args...)
}
