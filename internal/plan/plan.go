// Package plan decides, for one checkout, which services run there and where
// every upstream of a local service points (baseline or local).
package plan

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/glob"
)

// Target is where an upstream resolves.
type Target string

const (
	Baseline Target = "baseline"
	Local    Target = "local"
)

// ParseTarget accepts "baseline" or "local".
func ParseTarget(s string) (Target, error) {
	switch Target(s) {
	case Baseline, Local:
		return Target(s), nil
	}
	return "", fmt.Errorf("route target %q: want baseline or local", s)
}

// Routes overrides upstream resolution: Routes[service][upstreamService].
type Routes map[string]map[string]Target

// Set records one override.
func (r Routes) Set(service, upstream string, t Target) {
	if r[service] == nil {
		r[service] = map[string]Target{}
	}
	r[service][upstream] = t
}

// ParseRoute parses "web-b.api=baseline".
func ParseRoute(s string) (service, upstream string, t Target, err error) {
	lhs, rhs, ok := strings.Cut(s, "=")
	if !ok {
		return "", "", "", fmt.Errorf("route %q: want service.upstream=baseline|local", s)
	}
	service, upstream, ok = strings.Cut(lhs, ".")
	if !ok {
		return "", "", "", fmt.Errorf("route %q: want service.upstream=baseline|local", s)
	}
	t, err = ParseTarget(rhs)
	return service, upstream, t, err
}

// Request is the input for one worktree.
type Request struct {
	Slot int
	// Changed is used when Layers is empty: one worktree, these paths.
	Changed []string
	// Layers is a stack of worktrees, bottom first; the last one is where
	// hydra runs. Each layer's Changed is its diff against its parent only.
	Layers  []Layer
	With    []string // force these to run locally
	Without []string // force these to come from the baseline
	Routes  Routes
}

// Layer is one worktree of a stack.
type Layer struct {
	Name    string // branch name, for messages
	Root    string // worktree root; "" means the renderer's default
	Changed []string
}

// Upstream is one resolved env var of a local service.
type Upstream struct {
	Env     string
	Service string
	Target  Target
	Value   string
	Why     string
}

// Service is one row of the plan.
type Service struct {
	Name      string
	Local     bool
	Why       string // why it runs locally, or why it doesn't
	Port      int    // port of the instance this checkout uses (0 = none)
	PortEnv   string
	Prepare   string // rendered prepare command, local services only
	Disabled  bool   // process-compose `disabled: true`: listed, started by hand
	Upstreams []Upstream

	// Source is the process in the stack file this row runs; it differs
	// from Name for a follow service copied into another worktree.
	Source string
	// Root is the worktree the service runs from ("" = the default), and
	// Layer that worktree's branch.
	Root  string
	Layer string
	// FollowDeps renames depends_on entries to the copy of a follow service
	// that runs in this row's worktree.
	FollowDeps map[string]string
}

// Plan is the result for one checkout.
type Plan struct {
	Slot     int
	Layers   []Layer   // the stack, bottom first (one entry when not stacked)
	Services []Service // sorted by name
	Warnings []string
}

// Local returns the names of services that run in this checkout.
func (p *Plan) Local() []string {
	var out []string
	for _, s := range p.Services {
		if s.Local {
			out = append(out, s.Name)
		}
	}
	return out
}

// Get returns the plan row for a service.
func (p *Plan) Get(name string) *Service {
	for i := range p.Services {
		if p.Services[i].Name == name {
			return &p.Services[i]
		}
	}
	return nil
}

// Port returns the port a service listens on in a given slot.
func Port(st *config.Stack, s *config.Service, slot int) int {
	if s.Port == 0 {
		return 0
	}
	return s.Port + slot*st.Hydra.SlotStride
}

// Value renders what an env var pointing at service s receives in a slot.
// tmpl overrides s.URL when non-empty.
func Value(st *config.Stack, s *config.Service, slot int, tmpl string) string {
	if tmpl == "" {
		tmpl = s.URL
		if slot == 0 && s.BaselineURL != "" {
			tmpl = s.BaselineURL
		}
	}
	return expand(st, s, slot, tmpl, "")
}

func expand(st *config.Stack, s *config.Service, slot int, tmpl, value string) string {
	return strings.NewReplacer(
		"{host}", st.Hydra.Host,
		"{port}", strconv.Itoa(Port(st, s, slot)),
		"{slot}", strconv.Itoa(slot),
		"{value}", value,
	).Replace(tmpl)
}

// BaselinePlan plans the baseline checkout: everything, in slot 0.
func BaselinePlan(st *config.Stack) *Plan {
	p := &Plan{Slot: 0}
	for _, name := range st.Names() {
		s := st.Services[name]
		row := Service{Name: name, Source: name, Local: true, Why: "baseline", Port: Port(st, s, 0), PortEnv: s.PortEnv}
		for _, env := range s.UpstreamEnvs() {
			u := s.Upstreams[env]
			t := st.Services[u.Service]
			row.Upstreams = append(row.Upstreams, Upstream{
				Env: env, Service: t.Name, Target: Baseline, Value: Value(st, t, 0, u.Value), Why: "baseline",
			})
		}
		p.Services = append(p.Services, row)
	}
	return p
}

// Build plans a worktree.
func Build(st *config.Stack, req Request) (*Plan, error) {
	if req.Slot < 1 || req.Slot > st.Hydra.MaxSlots {
		return nil, fmt.Errorf("worktree slot must be 1..%d (0 is the baseline), got %d", st.Hydra.MaxSlots, req.Slot)
	}
	for _, n := range append(append([]string{}, req.With...), req.Without...) {
		if st.Services[n] == nil {
			return nil, fmt.Errorf("unknown service %q", n)
		}
	}
	for svc, ups := range req.Routes {
		if st.Services[svc] == nil {
			return nil, fmt.Errorf("route: unknown service %q", svc)
		}
		for up := range ups {
			if !dependsOn(st.Services[svc], up) {
				return nil, fmt.Errorf("route: %s has no upstream pointing at %q", svc, up)
			}
		}
	}

	layers := req.Layers
	if len(layers) == 0 {
		layers = []Layer{{Changed: req.Changed}}
	}
	top := len(layers) - 1
	p := &Plan{Slot: req.Slot, Layers: layers}
	owner := map[string]int{} // local service -> index of the layer it runs from
	without := set(req.Without)
	why := map[string]string{}     // services that run here -> reason
	skipped := map[string]string{} // services that wanted to run here but can't

	want := func(n, reason string) bool {
		if _, ok := why[n]; ok {
			return false
		}
		if _, ok := skipped[n]; ok {
			return false
		}
		s := st.Services[n]
		switch {
		case s.Shared:
			skipped[n] = "shared, always baseline (" + reason + ")"
			if strings.HasPrefix(reason, "changed") {
				p.Warnings = append(p.Warnings, fmt.Sprintf(
					"%s is shared, but its paths %s; the baseline's copy is used", n, reason))
			}
			return false
		case without[n]:
			skipped[n] = "excluded (--without), " + reason
			return false
		}
		why[n] = reason
		return true
	}

	// 1. Changed paths, from the top layer down: a service runs from the
	//    topmost worktree that changed it, which carries the lower layers'
	//    commits plus its own edits. Global paths only touch services that
	//    declare paths of their own; the rest are infra.
	for i := top; i >= 0; i-- {
		in := ""
		if len(layers) > 1 {
			in = " in " + layers[i].Name
		}
		for _, name := range st.Names() {
			s := st.Services[name]
			if len(s.Paths) == 0 {
				continue
			}
			if _, done := why[name]; done {
				continue
			}
			if f, ok := firstMatch(s.Paths, layers[i].Changed); ok {
				if want(name, "changed"+in+": "+f) {
					owner[name] = i
				}
			} else if f, ok := firstMatch(st.Hydra.GlobalPaths, layers[i].Changed); ok && !s.Shared {
				if want(name, "changed"+in+": "+f+" (global)") {
					owner[name] = i
				}
			}
		}
	}
	// 2. Explicit --with.
	for _, n := range req.With {
		want(n, "requested")
	}
	// 3. Route overrides start the service they're on; `=local` also starts
	//    the target. 4. Follow services start when a local service depends
	//    on them. Repeat until nothing changes.
	for changed := true; changed; {
		changed = false
		for _, svc := range sortedKeys(req.Routes) {
			changed = want(svc, "has a route override") || changed
			if _, ok := why[svc]; !ok {
				continue
			}
			for _, up := range sortedKeys(req.Routes[svc]) {
				if req.Routes[svc][up] == Local {
					changed = want(up, "routed to from "+svc) || changed
				}
			}
		}
		for _, name := range sortedKeys(why) {
			for _, d := range st.Services[name].DependsOn {
				if ds := st.Services[d]; ds != nil && ds.Follow {
					changed = want(d, "needed by "+name) || changed
				}
			}
		}
	}

	// Everything started for another reason runs from the top worktree.
	for n := range why {
		if _, ok := owner[n]; !ok && !st.Services[n].Follow {
			owner[n] = top
		}
	}
	// A follow service runs once per worktree that has a local service
	// depending on it: each worktree needs its own `pnpm install`. The copy
	// in the top worktree keeps the name; the others get the branch as a
	// suffix, and their dependents are pointed at them.
	followCopies := map[string]map[int]string{}  // follow service -> layer -> process name
	followDeps := map[string]map[string]string{} // dependent -> follow service -> copy name
	for _, f := range sortedKeys(why) {
		if !st.Services[f].Follow {
			continue
		}
		copies := map[int]string{}
		for _, d := range sortedKeys(why) {
			ds := st.Services[d]
			if ds.Follow || !contains(ds.DependsOn, f) {
				continue
			}
			i := owner[d]
			name := f
			if i != top {
				name = f + "-" + slug(layers[i].Name)
			}
			copies[i] = name
			if name != f {
				if followDeps[d] == nil {
					followDeps[d] = map[string]string{}
				}
				followDeps[d][f] = name
			}
		}
		if len(copies) == 0 { // asked for directly
			copies[top] = f
		}
		followCopies[f] = copies
	}

	for _, name := range st.Names() {
		s := st.Services[name]
		reason, local := why[name]
		if local && s.Follow {
			for _, i := range sortedInts(followCopies[name]) {
				p.Services = append(p.Services, Service{
					Name: followCopies[name][i], Source: name, Local: true, Why: reason,
					PortEnv: s.PortEnv, Disabled: disabled(st, name),
					Root: layers[i].Root, Layer: layers[i].Name,
				})
			}
			continue
		}
		row := Service{Name: name, Source: name, Local: local, Why: reason, PortEnv: s.PortEnv, Disabled: disabled(st, name)}
		if local {
			row.Root, row.Layer = layers[owner[name]].Root, layers[owner[name]].Name
			row.FollowDeps = followDeps[name]
		}
		if !local {
			row.Why = skipped[name]
			if row.Why == "" {
				row.Why = "unchanged"
				if s.Shared {
					row.Why = "shared"
				}
			}
			row.Port = Port(st, s, 0)
			p.Services = append(p.Services, row)
			continue
		}
		row.Port = Port(st, s, req.Slot)
		if s.Prepare != "" {
			row.Prepare = expand(st, s, req.Slot, s.Prepare, Value(st, s, req.Slot, ""))
		}
		for _, env := range s.UpstreamEnvs() {
			u := s.Upstreams[env]
			t := st.Services[u.Service]
			_, tLocal := why[t.Name]
			target, how := Baseline, "default: "+t.Name+" not running here"
			if tLocal {
				target, how = Local, "default: "+t.Name+" running here"
			}
			if o, ok := req.Routes[name][t.Name]; ok {
				if o == Local && !tLocal {
					return nil, fmt.Errorf("route %s.%s=local, but %s can't run here (%s)", name, t.Name, t.Name, skipped[t.Name])
				}
				target, how = o, "route override"
			}
			slot := 0
			if target == Local {
				slot = req.Slot
			}
			row.Upstreams = append(row.Upstreams, Upstream{
				Env: env, Service: t.Name, Target: target, Value: Value(st, t, slot, u.Value), Why: how,
			})
		}
		p.Services = append(p.Services, row)
	}
	sort.Slice(p.Services, func(i, j int) bool { return p.Services[i].Name < p.Services[j].Name })
	return p, nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func sortedInts(m map[int]string) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

// slug turns a branch name into something safe in a process name.
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func disabled(st *config.Stack, name string) bool {
	procs, _ := st.Raw["processes"].(map[string]any)
	proc, _ := procs[name].(map[string]any)
	d, _ := proc["disabled"].(bool)
	return d
}

func dependsOn(s *config.Service, target string) bool {
	for _, u := range s.Upstreams {
		if u.Service == target {
			return true
		}
	}
	return false
}

// firstMatch returns the first path that matches one of the patterns and
// none of the `!`-prefixed exclusions.
func firstMatch(patterns, paths []string) (string, bool) {
	for _, f := range paths {
		hit := false
		for _, pat := range patterns {
			if ex, ok := strings.CutPrefix(pat, "!"); ok {
				if glob.Match(ex, f) {
					hit = false
					break
				}
			} else if !hit && glob.Match(pat, f) {
				hit = true
			}
		}
		if hit {
			return f, true
		}
	}
	return "", false
}

func set(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
