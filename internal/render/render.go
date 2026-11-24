// Package render turns a plan into a plain process-compose file.
package render

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/plan"
)

// Options for rendering.
type Options struct {
	// Dir is the checkout hydra runs in. Services run from it unless the
	// plan puts them in another worktree of the stack; logs always go here.
	Dir string
	// Watch, when set, is the command of a `hydra-watch` process added to
	// the file, which keeps the running project in step with new changes.
	Watch string
	// Router, when set, is the command of a `hydra-router` process added to
	// the baseline, through which routed services reach each worktree.
	Router string
	// NodeHook is the path of hydra's Node preload, loaded into services
	// marked `routed: node`.
	NodeHook string
	// CAFile is the CA behind the baseline's TLS fronts. A baseline process
	// that is given a front's address gets it as NODE_EXTRA_CA_CERTS, so
	// Node accepts the front.
	CAFile string
}

const idle = "while :; do sleep 3600; done"

// WatchAnchor is an empty directory in each worktree, relative to its root,
// which hydra-watch's own `watch` points at (see Render). Whoever writes the
// rendered file creates it.
const WatchAnchor = ".process-compose/watch-anchor"

// ReadyName is the process that ends once a worktree's own database, the
// service named, is started and migrated. What reads that database waits for
// it instead of for the service.
func ReadyName(service string) string { return "hydra-ready-" + service }

// ownDatabases lists the services of the plan that are a worktree's own
// Supabase stack: the ones hydra wraps, and so can tell when they are ready.
func ownDatabases(st *config.Stack, p *plan.Plan) map[string]plan.Service {
	out := map[string]plan.Service{}
	if p.Slot == 0 {
		return out
	}
	procs, _ := st.Raw["processes"].(map[string]any)
	for _, row := range p.Services {
		svc := st.Services[row.Source]
		proc, _ := procs[row.Source].(map[string]any)
		_, runs := proc["command"].(string)
		if row.Local && !row.Disabled && svc != nil && svc.Kind == config.KindSupabase && runs {
			out[row.Name] = row
		}
	}
	return out
}

// reads reports whether a local service is given the address of db here.
func reads(row plan.Service, db string) bool {
	for _, u := range row.Upstreams {
		if u.Service == db && u.Target == plan.Local {
			return true
		}
	}
	return false
}

// anyWatch reports whether a process of the stack restarts on file changes.
func anyWatch(st *config.Stack) bool {
	procs, _ := st.Raw["processes"].(map[string]any)
	for _, raw := range procs {
		if proc, ok := raw.(map[string]any); ok {
			if _, has := proc["watch"]; has {
				return true
			}
		}
	}
	return false
}

// Render returns a process-compose document with the plan's local services,
// `x-hydra` keys removed, and ports and upstream values injected as
// environment variables.
//
// A local service that depends on a service running in the baseline gets a
// stand-in process with that name: it runs nothing and copies the original's
// readiness probe or ready log line, so `process_healthy` waits for the
// baseline's copy to be healthy instead of starting a second one.
func Render(st *config.Stack, p *plan.Plan, opt Options) ([]byte, error) {
	dir, err := filepath.Abs(opt.Dir)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for k, v := range st.Raw {
		if k != "x-hydra" && k != "processes" {
			out[k] = v
		}
	}

	rawProcs, _ := st.Raw["processes"].(map[string]any)
	local := map[string]bool{}
	for _, n := range p.Local() {
		local[n] = true
	}

	procs := map[string]any{}
	stubs := map[string]map[string]any{}
	exe := st.Hydra.Exe
	if exe == "" {
		exe = "hydra"
	}
	// A worktree's own database answers before it is usable: a new stack is
	// still applying migrations and seeds, a resumed one the migrations the
	// branch gained. A service started then finds tables missing. So what
	// reads the database waits for a step that ends when hydra has started
	// and migrated it, not for the database's own readiness.
	dbs := ownDatabases(st, p)
	for name, row := range dbs {
		root := dir
		if row.Root != "" {
			root = row.Root
		}
		procs[ReadyName(name)] = map[string]any{
			"namespace":   "hydra",
			"description": "Ends once this worktree's " + name + " is started and migrated",
			"command":     "exec " + shellQuote(exe) + " prepare supabase --ready",
			"working_dir": root,
			"environment": []string{"HYDRA_SERVICE=" + name, "HYDRA_SLOT=" + strconv.Itoa(p.Slot)},
		}
	}
	for _, row := range p.Services {
		if !row.Local {
			continue
		}
		source := row.Source
		if source == "" {
			source = row.Name
		}
		root := dir
		if row.Root != "" {
			root = row.Root
		}
		src, _ := rawProcs[source].(map[string]any)
		proc := map[string]any{}
		for k, v := range src {
			if k != "x-hydra" {
				proc[k] = v
			}
		}

		// The rendered file lives in .process-compose/, and process-compose
		// resolves some paths against the file's directory: make them absolute.
		// Code paths resolve in the service's own worktree, so a stacked
		// service also restarts on edits made in that worktree.
		proc["working_dir"] = abs(root, proc["working_dir"])
		if ll, ok := proc["log_location"].(string); ok {
			if row.Name != source { // a copy needs its own log file
				ext := filepath.Ext(ll)
				ll = strings.TrimSuffix(ll, ext) + strings.TrimPrefix(row.Name, source) + ext
			}
			proc["log_location"] = abs(dir, ll)
		}
		if w, ok := proc["watch"].(map[string]any); ok {
			proc["watch"] = absWatch(root, w)
		}
		if !row.Disabled {
			delete(proc, "disabled") // asked for by name: `hydra up mobile`
		}
		if p.Slot == 0 {
			// The main checkout's code changes only on `hydra update`, which
			// restarts what the new commits touched, in order. A file
			// watcher would restart a service in the middle of a pull,
			// before its migrations ran.
			delete(proc, "watch")
			if cmd := st.Services[source].BaselineCommand; cmd != "" {
				proc["command"] = cmd
			}
		}
		// Probes written against the baseline port must follow the service
		// to its slot port, or they'd report the baseline's copy as healthy.
		if base := st.Services[source].Port; base != 0 && row.Port != base {
			for _, k := range []string{"readiness_probe", "liveness_probe"} {
				if pr, ok := proc[k].(map[string]any); ok {
					proc[k] = shiftProbe(pr, base, row.Port)
				}
			}
		}

		deps := map[string]any{} // a copy: st.Raw must stay untouched
		if src, ok := proc["depends_on"].(map[string]any); ok {
			for k, v := range src {
				if copyName, ok := row.FollowDeps[k]; ok {
					k = copyName
				}
				deps[k] = v
			}
		}
		for db := range dbs {
			if _, waits := deps[db]; db != row.Name && (waits || reads(row, db)) {
				delete(deps, db)
				deps[ReadyName(db)] = map[string]any{"condition": "process_completed_successfully"}
			}
		}
		for d, cond := range deps {
			if !local[d] && stubs[d] == nil {
				stubs[d] = stub(d, rawProcs[d], cond)
			}
		}
		if row.Prepare != "" {
			name := "hydra-prepare-" + row.Name
			procs[name] = map[string]any{
				"namespace":   "hydra",
				"description": "Prepares " + row.Name + " for slot " + strconv.Itoa(p.Slot),
				"command":     row.Prepare,
				"working_dir": root,
				"depends_on":  prepareDeps(deps),
				// What the step makes is named after the slot; HYDRA_ROOT is
				// the worktree that holds the slot now.
				"environment": []string{"HYDRA_SERVICE=" + row.Name, "HYDRA_SLOT=" + strconv.Itoa(p.Slot), "HYDRA_ROOT=" + dir},
			}
			deps[name] = map[string]any{"condition": "process_completed_successfully"}
		}
		if len(deps) > 0 {
			proc["depends_on"] = deps
		}

		env, err := envList(proc["environment"])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", row.Name, err)
		}
		env = append(env, "HYDRA_SERVICE="+row.Name, "HYDRA_SLOT="+strconv.Itoa(p.Slot))
		if row.Port != 0 && st.Services[source].Kind != config.KindSupabase { // the CLI takes its ports from SUPABASE_*_PORT
			env = append(env, row.PortEnv+"="+strconv.Itoa(row.Port))
		}
		var names []string
		for _, u := range row.Upstreams {
			env = append(env, u.Env+"="+u.Value)
			names = append(names, u.Env)
		}
		env = append(env, "HYDRA_UPSTREAMS="+strings.Join(names, ","))
		if svc := st.Services[source]; svc.Kind == config.KindSupabase && p.Slot > 0 {
			// The worktree's own Supabase stack. TMPDIR inside the checkout:
			// the CLI bind-mounts temp files into containers, and Colima's VM
			// only sees the home directory, not macOS's /var/folders.
			tmp := filepath.Join(root, "supabase", ".temp", "tmp")
			env = append(env, st.SupabaseEnv(svc, p.Slot)...)
			env = append(env, "TMPDIR="+tmp)
			if cmd, ok := proc["command"].(string); ok {
				// Around the stack file's command: hydra first stops the stack
				// this directory has for another branch (it would hold the
				// ports), and afterwards applies new migrations and puts the
				// checkout back, also when the start failed.
				hydra := shellQuote(exe) + " prepare supabase"
				proc["command"] = "mkdir -p " + shellQuote(tmp) + " && " + hydra + " --before || exit 1\n(\n" +
					strings.TrimRight(cmd, "\n") + "\n)\n" + hydra + " --started $?"
			}
			// The stop must reach this stack too, never the baseline's: name
			// the stack's environment in the command itself rather than rely
			// on it being inherited.
			if sd, ok := proc["shutdown"].(map[string]any); ok {
				if cmd, ok := sd["command"].(string); ok {
					c := map[string]any{}
					for k, v := range sd {
						c[k] = v
					}
					var exports []string
					for _, kv := range append(st.SupabaseEnv(svc, p.Slot), "TMPDIR="+tmp) {
						k, v, _ := strings.Cut(kv, "=")
						exports = append(exports, k+"="+shellQuote(v))
					}
					// After the stack file's stop, hydra stops what is left of
					// this directory's stacks: the CLI's own stop misses one
					// that was made before the branch was renamed.
					c["command"] = "export " + strings.Join(exports, " ") + "\n" + strings.TrimRight(cmd, "\n") + "\n" + shellQuote(exe) + " prepare supabase --stopped"
					proc["shutdown"] = c
				}
			}
		}
		if svc := st.Services[source]; p.Slot == 0 && svc.Routed != config.NotRouted && opt.Router != "" {
			// One copy for every worktree: its calls go through the router,
			// which has to be up first.
			env = append(env, "HYDRA_ROUTED_ORIGINS="+strings.Join(routedOrigins(st, svc), ","))
			var per []string
			for _, e := range svc.UpstreamEnvs() {
				if svc.Upstreams[e].PerRequest {
					per = append(per, e)
				}
			}
			if len(per) > 0 {
				env = append(env, "HYDRA_PER_REQUEST="+strings.Join(per, ","))
			}
			if svc.Routed == config.RoutedNode && opt.NodeHook != "" {
				env = withNodeHook(env, opt.NodeHook)
			}
			deps["hydra-router"] = map[string]any{"condition": "process_started"}
			proc["depends_on"] = deps
		}
		if p.Slot == 0 && usesFront(st.Services[source]) {
			// A process told to use a TLS front needs the router up first,
			// and the CA to accept the front with. Only such a process is
			// touched: adding a front to a stack restarts nothing else.
			if _, has := deps["hydra-router"]; !has && opt.Router != "" {
				deps["hydra-router"] = map[string]any{"condition": "process_started"}
				proc["depends_on"] = deps
			}
			if opt.CAFile != "" && !hasEnv(env, "NODE_EXTRA_CA_CERTS") {
				env = append(env, "NODE_EXTRA_CA_CERTS="+opt.CAFile)
			}
		}
		proc["environment"] = env

		procs[row.Name] = proc
	}
	if opt.Watch != "" {
		w := map[string]any{
			"namespace":    "hydra",
			"description":  "Starts services here as soon as their paths change",
			"command":      opt.Watch,
			"working_dir":  dir,
			"availability": map[string]any{"restart": "on_failure", "backoff_seconds": 5},
		}
		if anyWatch(st) {
			// process-compose builds its file watcher only when a process has
			// a `watch` as the project starts, and a worktree often starts
			// with nothing of its own to run. A service hydra-watch adds
			// later would then never restart on a change to its files. This
			// watch, on a directory nothing writes to, never fires: it is
			// there so the watcher exists.
			w["watch"] = map[string]any{"paths": []any{map[string]any{
				"path":    filepath.Join(dir, WatchAnchor),
				"include": []any{"nothing-is-ever-written-here"},
			}}}
		}
		procs["hydra-watch"] = w
	}
	if opt.Router != "" && p.Slot == 0 {
		procs["hydra-router"] = map[string]any{
			"namespace":    "hydra",
			"description":  "Lets worktrees use this checkout's copy of the routed services",
			"command":      opt.Router,
			"working_dir":  dir,
			"availability": map[string]any{"restart": "always", "backoff_seconds": 2},
		}
	}
	for name, s := range stubs {
		if _, taken := procs[name]; !taken {
			procs[name] = s
		}
	}
	out["processes"] = procs

	header := fmt.Sprintf("# Generated by hydra for slot %d from the x-hydra fields. Do not edit.\n", p.Slot)
	if p.Slot > 0 {
		header += "# Processes in the `main` namespace run nothing; they wait for the main checkout's copy.\n"
	}
	body, err := yaml.Marshal(out)
	if err != nil {
		return nil, err
	}
	return append([]byte(header), body...), nil
}

// routedOrigins lists the router addresses a routed service calls, which is
// where hydra's hook adds the worktree tag.
func routedOrigins(st *config.Stack, s *config.Service) []string {
	seen := map[string]bool{}
	var out []string
	for _, env := range s.UpstreamEnvs() {
		u := s.Upstreams[env]
		if !st.Dispatches(s, u) {
			continue
		}
		o := "http://" + st.Hydra.Host + ":" + strconv.Itoa(st.DispatchPort(st.Services[u.Service]))
		if !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	return out
}

// usesFront reports whether the baseline's copy of s is given a TLS front's
// address.
func usesFront(s *config.Service) bool {
	for _, u := range s.Upstreams {
		if strings.Contains(u.BaselineValue, "{tls}") {
			return true
		}
	}
	return false
}

func hasEnv(env []string, name string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			return true
		}
	}
	return false
}

// withNodeHook adds `--require <hook>` to NODE_OPTIONS, keeping what the
// process already sets there.
func withNodeHook(env []string, hook string) []string {
	flag := "--require " + hook
	if strings.ContainsAny(hook, " \t") {
		flag = `--require "` + hook + `"`
	}
	for i, kv := range env {
		if v, ok := strings.CutPrefix(kv, "NODE_OPTIONS="); ok {
			env[i] = "NODE_OPTIONS=" + strings.TrimSpace(v+" "+flag)
			return env
		}
	}
	return append(env, "NODE_OPTIONS="+flag)
}

// stub builds the stand-in for a baseline process that a local one waits on.
func stub(name string, raw any, cond any) map[string]any {
	src, _ := raw.(map[string]any)
	s := map[string]any{
		"namespace":   "main",
		"description": "Stand-in: waits for main's " + name,
		"command":     idle,
	}
	c, _ := cond.(map[string]any)
	switch c["condition"] {
	case "process_completed_successfully", "process_completed":
		s["command"] = "true"
	case "process_log_ready":
		if line, ok := src["ready_log_line"].(string); ok {
			s["command"] = "echo " + shellQuote(line) + "; " + idle
			s["ready_log_line"] = line
		}
	case "process_healthy":
		if rp, ok := src["readiness_probe"]; ok {
			s["readiness_probe"] = rp
		}
	}
	return s
}

// prepareDeps makes a prepare step wait for the same healthy dependencies
// as its service (say, Temporal), but nothing else.
func prepareDeps(deps map[string]any) map[string]any {
	out := map[string]any{}
	keys := make([]string, 0, len(deps))
	for k := range deps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if c, _ := deps[k].(map[string]any); c["condition"] == "process_healthy" {
			out[k] = deps[k]
		}
	}
	return out
}

// shiftProbe copies a probe, moving http_get.port from base to port.
func shiftProbe(pr map[string]any, base, port int) map[string]any {
	out := map[string]any{}
	for k, v := range pr {
		out[k] = v
	}
	if hg, ok := pr["http_get"].(map[string]any); ok {
		c := map[string]any{}
		for k, v := range hg {
			c[k] = v
		}
		if p, ok := c["port"].(int); ok && p == base {
			c["port"] = port
		}
		out["http_get"] = c
	}
	return out
}

func abs(dir string, v any) string {
	p, _ := v.(string)
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

// absWatch copies a watch block with each paths[].path made absolute.
func absWatch(dir string, w map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range w {
		out[k] = v
	}
	if paths, ok := w["paths"].([]any); ok {
		var ps []any
		for _, p := range paths {
			m, ok := p.(map[string]any)
			if !ok {
				ps = append(ps, p)
				continue
			}
			c := map[string]any{}
			for k, v := range m {
				c[k] = v
			}
			if s, ok := c["path"].(string); ok {
				c["path"] = abs(dir, s)
			}
			ps = append(ps, c)
		}
		out["paths"] = ps
	}
	return out
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func envList(v any) ([]string, error) {
	switch e := v.(type) {
	case nil:
		return nil, nil
	case []any:
		out := make([]string, 0, len(e))
		for _, x := range e {
			s, ok := x.(string)
			if !ok {
				return nil, fmt.Errorf("environment entries must be strings, got %T", x)
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("environment must be a list of KEY=VALUE strings, got %T", v)
}
