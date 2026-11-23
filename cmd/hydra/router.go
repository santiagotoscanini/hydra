package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/plan"
	"github.com/santiagotoscanini/hydra/internal/router"
	"github.com/santiagotoscanini/hydra/internal/routes"
)

// routerCommand is the command line of the baseline's hydra-router process.
func routerCommand(t *target) (string, error) {
	exe, err := stableExecutable()
	if err != nil {
		return "", err
	}
	args := []string{exe, "router", "--root", t.checkout.Root, "-f", t.file}
	for i, a := range args {
		args[i] = shQuote(a)
	}
	return "exec " + strings.Join(args, " "), nil
}

// routerCmd runs inside the baseline's process-compose project. It reads the
// stack file and the routes each running worktree published, and keeps its
// listeners in step with them (see package router).
func routerCmd(args []string) error {
	fs := flag.NewFlagSet("router", flag.ContinueOnError)
	file := fs.String("f", "", "")
	root := fs.String("root", ".", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	co, err := gitx.Find(*root)
	if err != nil {
		return err
	}
	if *file == "" {
		*file = defaultFile(co)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	r := &router.Router{
		Load: func() (*router.State, error) { return routerState(co.CommonDir, *file) },
		Logf: logf,
	}
	logf("routing for %s", co.Root)
	return r.Run(ctx)
}

// routerState is the stack file plus the routes of every worktree whose
// process-compose server answers.
func routerState(commonDir, file string) (*router.State, error) {
	st, err := config.Load(file)
	if err != nil {
		return nil, err
	}
	state := &router.State{Host: st.Hydra.Host, Services: map[string]router.Service{}, Worktrees: map[string]router.Worktree{}, Fronts: map[string]router.Front{}}
	if st.HasTLS() {
		// A front without a certificate can't open; say why and keep routing.
		files, err := tlsFilesFor(&target{file: file, stack: st})
		if err == nil {
			state.Cert, err = frontCert(files)
		}
		if err != nil {
			warnOnce("tls", fmt.Sprintf("no certificate for the TLS fronts: %v", err))
		}
		for name, s := range st.Services {
			if s.TLS != nil {
				state.Fronts[name] = router.Front{Listen: s.TLS.Port, Target: s.Port, ALPN: s.TLS.ALPN}
			}
		}
	}
	dispatched := map[string]bool{}
	for _, name := range st.Dispatched() {
		dispatched[name] = true
	}
	for name, s := range st.Services {
		if s.Port == 0 {
			continue
		}
		svc := router.Service{Port: s.Port, Probe: s.Routed == config.RoutedNode}
		if dispatched[name] {
			svc.Dispatch = st.DispatchPort(s)
		}
		state.Services[name] = svc
	}
	files, err := routes.Read(commonDir)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if !f.Live() {
			continue
		}
		wt := router.Worktree{Name: f.Name, Local: f.Local, Entries: map[string]router.Entry{}}
		for name, e := range f.Entries {
			// The stack file may have changed since the worktree wrote this.
			if s := st.Services[name]; s != nil && s.Routed != config.NotRouted {
				wt.Entries[name] = router.Entry{Port: e.Port, Upstreams: e.Upstreams}
			}
		}
		state.Worktrees[f.Tag()] = wt
	}
	return state, nil
}

// warned is what the router last said about each subject, so a problem it
// meets twice a second is logged once.
var warned = map[string]string{}

func warnOnce(key, msg string) {
	if warned[key] != msg {
		warned[key] = msg
		logf("%s", msg)
	}
}

// publishRoutes tells the baseline's router what this worktree runs, and
// waits for the router to let go of any port a service here is about to
// take: the worktree's port for a routed service is the router's until that
// service starts running here.
func publishRoutes(t *target, p *plan.Plan) error {
	if p.Slot == 0 || !t.stack.HasRouted() {
		return nil
	}
	f := routes.File{
		Slot: p.Slot, Root: t.checkout.Root, Socket: socketPath(t.checkout), Name: filepath.Base(t.checkout.Root),
		Local: map[string]int{}, Entries: map[string]routes.Entry{},
	}
	if n := len(p.Layers); n > 0 && p.Layers[n-1].Name != "" {
		f.Name = p.Layers[n-1].Name
	}
	var taking []int
	for _, row := range p.Services {
		switch {
		case row.Local && row.Port != 0:
			f.Local[row.Name] = row.Port
			if s := t.stack.Services[row.Source]; s != nil && s.Routed != config.NotRouted {
				taking = append(taking, row.Port)
			}
		case row.Routed:
			e := routes.Entry{Port: row.Port}
			for _, u := range row.Upstreams {
				if u.PerRequest {
					if e.Upstreams == nil {
						e.Upstreams = map[string]string{}
					}
					e.Upstreams[u.Env] = u.Value
				}
			}
			f.Entries[row.Name] = e
		}
	}
	changed, err := routes.Write(t.checkout.CommonDir, f)
	if err != nil || !changed {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for _, port := range taking {
		addr := net.JoinHostPort(t.stack.Hydra.Host, strconv.Itoa(port))
		for time.Now().Before(deadline) {
			c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
			if err != nil {
				break
			}
			c.Close()
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
}

// routedNote lists the routed services a worktree uses, with their address.
func routedNote(t *target, p *plan.Plan) {
	for _, row := range p.Services {
		if row.Routed {
			fmt.Fprintf(os.Stderr, "hydra: %s is main's copy, serving this worktree at http://%s:%d\n", row.Name, t.stack.Hydra.Host, row.Port)
		}
	}
}
