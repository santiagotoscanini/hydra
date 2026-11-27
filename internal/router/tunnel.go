package router

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
)

// Tunnel is where a public tunnel's requests go now. hydra works it out
// from who holds the tunnel; the router only follows it, request by
// request, so moving the tunnel needs no new listener.
type Tunnel struct {
	Listen int    // where the tunnel sends its requests
	To     string // the service they reach
	// Target is the port of the copy they go to, and Label names it
	// ("api@fix-cart", "api@main").
	Target int
	Label  string
	// Tag is the holder's worktree tag, put on every request so that routed
	// services downstream serve the holder too; "" for main.
	Tag string
	// Holder names the worktree that holds the tunnel; "" for main. Own says
	// Target is that worktree's own copy: when it doesn't answer, the
	// request fails rather than reach main's.
	Holder string
	Own    bool
	// Down, when set, is why no request can go through now (the holder's
	// stack isn't running): each gets a 502 saying so.
	Down string
}

type tunnelKey struct{}

// tunnel handles a tunnel's port. As on the other ports, a websocket upgrade
// or a stream goes through like any request, and the Host header (the
// tunnel's public name) is passed on as it came.
func (r *Router) tunnel(name string) http.Handler {
	proxy := &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			t := pr.In.Context().Value(tunnelKey{}).(Tunnel)
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = net.JoinHostPort(r.state.Load().Host, strconv.Itoa(t.Target))
			pr.Out.Host = pr.In.Host
			// What the tunnel says about the public request (https, the
			// caller's address) stays: a webhook's signature is checked
			// against the public URL.
			for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				if v := pr.In.Header.Values(h); len(v) > 0 {
					pr.Out.Header[h] = v
				}
			}
			stripHydra(pr.Out.Header) // only who holds the tunnel decides the tag
			if t.Tag != "" {
				pr.Out.Header.Set(TagHeader, t.Tag)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Set(ServedByHeader, resp.Request.Context().Value(tunnelKey{}).(Tunnel).Label)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			t := req.Context().Value(tunnelKey{}).(Tunnel)
			if t.Own {
				// Never main's instead: the developer moved the tunnel here to
				// see these requests.
				fail(w, http.StatusBadGateway, "tunnel %s is held by worktree %s, and its %s isn't answering on :%d (%v). There: `hydra up -D %s`, or give the tunnel back: `hydra tunnel release %s`",
					name, t.Holder, t.To, t.Target, err, t.To, name)
				return
			}
			fail(w, http.StatusBadGateway, "main's %s isn't answering on :%d (%v), and tunnel %s goes there. Is main's stack running?", t.To, t.Target, err, name)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		t, ok := r.state.Load().Tunnels[name]
		if !ok {
			fail(w, http.StatusNotFound, "no tunnel %s in the stack file any more", name)
			return
		}
		if t.Down != "" {
			fail(w, http.StatusBadGateway, "%s", t.Down)
			return
		}
		r.active(t.Tag)
		proxy.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), tunnelKey{}, t)))
	})
}
