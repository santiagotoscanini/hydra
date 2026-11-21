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
}

const idle = "while :; do sleep 3600; done"

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
		if row.Port != 0 {
			env = append(env, row.PortEnv+"="+strconv.Itoa(row.Port))
		}
		var names []string
		for _, u := range row.Upstreams {
			env = append(env, u.Env+"="+u.Value)
			names = append(names, u.Env)
		}
		env = append(env, "HYDRA_UPSTREAMS="+strings.Join(names, ","))
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
		proc["environment"] = env

		procs[row.Name] = proc
	}
	if opt.Watch != "" {
		procs["hydra-watch"] = map[string]any{
			"namespace":    "hydra",
			"description":  "Starts services here as soon as their paths change",
			"command":      opt.Watch,
			"working_dir":  dir,
			"availability": map[string]any{"restart": "on_failure", "backoff_seconds": 5},
		}
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
		header += "# Processes in the `baseline` namespace run nothing; they wait for the baseline's copy.\n"
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
		if u.Service == s.Name || !st.Routable(u) {
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
		"namespace":   "baseline",
		"description": "Stand-in: waits for the baseline's " + name,
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
