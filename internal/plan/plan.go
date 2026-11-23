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
	// Routed is the baseline's copy of a routed service, reached through
	// this worktree's own port so its requests carry the worktree's tag.
	Routed Target = "routed"
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
	// MainChanged lists the paths that differ between the commit where this
	// branch left main and the commit the main checkout has checked out
	// now: what main gained since. With MainBehind it is the other way
	// round: the branch started from a newer commit than main is on, and
	// these are the paths main still lacks.
	MainChanged []string
	MainBehind  bool
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
	// PerRequest: the service picks this value per request itself, from
	// what hydra's hook hands it.
	PerRequest bool
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
	// Routed: the baseline's copy serves this worktree. Port is then the
	// worktree's port for it, where the router tags requests and passes them
	// on, and Upstreams say where that copy's calls go for this worktree.
	Routed bool
	// Dispatch: in the baseline, a routed service's calls go through the
	// router so each lands in the calling worktree. Lists the env vars set
	// to a router address.
	Dispatch []string

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

// Skew is a service this worktree uses from main whose code there isn't
// the code the branch was built against.
type Skew struct {
	Service string
	Why     string   // how this worktree uses it: "web here calls it"
	Files   []string // where main and the branch's starting point differ, sorted
}

// Plan is the result for one checkout.
type Plan struct {
	Slot     int
	Layers   []Layer   // the stack, bottom first (one entry when not stacked)
	Services []Service // sorted by name
	Warnings []string
	// OutOfStep lists what this worktree uses from main at another version
	// than its branch started from. MainBehind says which side is older:
	// false, the branch (merge main into it); true, main (update main).
	OutOfStep  []Skew
	MainBehind bool
	// Unclaimed are changed files that no service lists in its paths and
	// the stack doesn't ignore: nothing runs here because of them.
	Unclaimed []string
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
	return expandPort(st, s, Port(st, s, slot), slot, tmpl, value)
}

// expandPort fills in a template; {port} becomes port, and {port.NAME} the
// slot's copy of s's named port.
func expandPort(st *config.Stack, s *config.Service, port, slot int, tmpl, value string) string {
	pairs := []string{
		"{host}", st.Hydra.Host,
		"{port}", strconv.Itoa(port),
		"{slot}", strconv.Itoa(slot),
		"{value}", value,
	}
	for name, p := range s.Ports {
		pairs = append(pairs, "{port."+name+"}", strconv.Itoa(p+slot*st.Hydra.SlotStride))
	}
	return strings.NewReplacer(pairs...).Replace(tmpl)
}

// dispatchValue is what a routed service's env var receives in the baseline:
// the target's address with the router's port for it.
func dispatchValue(st *config.Stack, t *config.Service, tmpl string) string {
	if tmpl == "" {
		tmpl = t.URL
	}
	return expandPort(st, t, st.DispatchPort(t), 0, tmpl, "")
}

// BaselinePlan plans the baseline checkout: everything, in slot 0.
func BaselinePlan(st *config.Stack) *Plan {
	p := &Plan{Slot: 0}
	for _, name := range st.Names() {
		s := st.Services[name]
		row := Service{Name: name, Source: name, Local: true, Why: "baseline", Port: Port(st, s, 0), PortEnv: s.PortEnv, Disabled: disabled(st, name)}
		for _, env := range s.UpstreamEnvs() {
			u := s.Upstreams[env]
			t := st.Services[u.Service]
			up := Upstream{Env: env, Service: t.Name, Target: Baseline, Value: Value(st, t, 0, u.Value), Why: "baseline"}
			if st.Dispatches(s, u) {
				up.Value, up.Why = dispatchValue(st, t, u.Value), "through the router, per request"
				row.Dispatch = append(row.Dispatch, env)
			}
			row.Upstreams = append(row.Upstreams, up)
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
	type sharedHit struct{ name, reason, file string }
	var sharedHits []sharedHit
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
				if s.Shared {
					if _, seen := skipped[name]; !seen {
						sharedHits = append(sharedHits, sharedHit{name, "changed" + in + ": " + f, f})
					}
				}
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
		// 5. A service that reaches something running here through a value
		//    no proxy can route (a database URL, a Temporal namespace, a
		//    queue name) runs here too, or it would keep using the
		//    baseline's: main's api would read main's database and start
		//    workflows in main's namespace. HTTP calls are left alone: a
		//    routed service routes them, and a plain one keeps calling the
		//    baseline as always. A per_request value is told apart by the
		//    routed service itself.
		for _, name := range st.Names() {
			s := st.Services[name]
			if _, here := why[name]; here {
				continue
			}
			routed := s.Routed != config.NotRouted
			for _, env := range s.UpstreamEnvs() {
				u := s.Upstreams[env]
				if _, tHere := why[u.Service]; !tHere || u.Service == name || u.PerRequest || st.Routable(u) {
					continue
				}
				reason := env + " points at " + u.Service + ", which runs here"
				if routed {
					reason = env + " points at " + u.Service + " here and can't be routed per request"
				}
				if _, known := skipped[name]; !known && without[name] {
					p.Warnings = append(p.Warnings, fmt.Sprintf(
						"%s is excluded, but its %s; those calls reach the baseline's %s", name, reason, u.Service))
				}
				changed = want(name, reason) || changed
				break
			}
		}
	}

	// A shared service's changed file is worth a warning only when nothing
	// running here takes it: a migration that this worktree's own database
	// applies needs none.
	for _, h := range sharedHits {
		taken := false
		for n := range why {
			if _, ok := firstMatch(st.Services[n].Paths, []string{h.file}); ok {
				taken = true
			}
		}
		if !taken {
			p.Warnings = append(p.Warnings, fmt.Sprintf(
				"%s is shared, but its paths %s; the baseline's copy is used", h.name, h.reason))
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

	// A process that is `disabled: true` waits for someone to start it. One
	// asked for by name (`hydra up mobile`) has been.
	off := func(name string) bool { return disabled(st, name) && !contains(req.With, name) }
	for _, name := range st.Names() {
		s := st.Services[name]
		reason, local := why[name]
		if local && s.Follow {
			for _, i := range sortedInts(followCopies[name]) {
				p.Services = append(p.Services, Service{
					Name: followCopies[name][i], Source: name, Local: true, Why: reason,
					PortEnv: s.PortEnv, Disabled: off(name),
					Root: layers[i].Root, Layer: layers[i].Name,
				})
			}
			continue
		}
		row := Service{Name: name, Source: name, Local: local, Why: reason, PortEnv: s.PortEnv, Disabled: off(name)}
		if local {
			row.Root, row.Layer = layers[owner[name]].Root, layers[owner[name]].Name
			row.FollowDeps = followDeps[name]
		}
		if !local && s.Routed != config.NotRouted {
			// The baseline's copy, behind this worktree's port.
			row.Routed, row.Port = true, Port(st, s, req.Slot)
			row.Why = "main's copy, tagged for this worktree"
			if sk := skipped[name]; sk != "" {
				row.Why += "; " + sk
			}
			for _, env := range s.UpstreamEnvs() {
				u := s.Upstreams[env]
				t := st.Services[u.Service]
				if t.Name == name || !(u.PerRequest || st.Dispatches(s, u)) {
					continue
				}
				up := Upstream{Env: env, Service: t.Name, Target: Baseline, Value: Value(st, t, 0, u.Value), Why: "per request: " + t.Name + " not running here", PerRequest: u.PerRequest}
				if _, tLocal := why[t.Name]; tLocal {
					up.Target, up.Value, up.Why = Local, Value(st, t, req.Slot, u.Value), "per request: "+t.Name+" running here"
				}
				row.Upstreams = append(row.Upstreams, up)
			}
			p.Services = append(p.Services, row)
			continue
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
		if tmpl := prepareTemplate(st, s); tmpl != "" {
			row.Prepare = expand(st, s, req.Slot, tmpl, Value(st, s, req.Slot, ""))
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
			if _, overridden := req.Routes[name][t.Name]; !tLocal && !overridden &&
				t.Routed != config.NotRouted && t.Name != name && st.Routable(u) {
				// Through this worktree's port for it, so the request is tagged.
				target, slot, how = Routed, req.Slot, "default: main's "+t.Name+" through this worktree's port"
			}
			row.Upstreams = append(row.Upstreams, Upstream{
				Env: env, Service: t.Name, Target: target, Value: Value(st, t, slot, u.Value), Why: how,
			})
		}
		p.Services = append(p.Services, row)
	}
	sort.Slice(p.Services, func(i, j int) bool { return p.Services[i].Name < p.Services[j].Name })
	p.OutOfStep, p.MainBehind = outOfStep(st, p, req.MainChanged), req.MainBehind
	for _, l := range layers {
		p.Unclaimed = append(p.Unclaimed, unclaimed(st, l.Changed)...)
	}
	p.Unclaimed = dedupe(p.Unclaimed)
	return p, nil
}

// outOfStep finds the services this worktree uses from main whose files are
// among those main changed since the branch left it. Only services that
// meet this worktree's code count: the ones a service running here calls or
// waits for (a migrations job changes the database it reads), and a routed
// service whose calls land in this worktree. Two services that both run in
// main are in step with each other whatever this branch did.
func outOfStep(st *config.Stack, p *Plan, mainChanged []string) []Skew {
	if len(mainChanged) == 0 {
		return nil
	}
	local := map[string]bool{}
	for _, row := range p.Services {
		if row.Local {
			local[row.Source] = true
		}
	}
	// For each of main's services: who here calls it, who here waits for
	// it, and whose calls it serves.
	callers, waiters, serves := map[string][]string{}, map[string][]string{}, map[string][]string{}
	add := func(m map[string][]string, svc, who string) {
		if !contains(m[svc], who) {
			m[svc] = append(m[svc], who)
		}
	}
	for _, row := range p.Services {
		switch {
		case row.Local && !row.Disabled:
			for _, u := range row.Upstreams {
				if u.Target != Local && u.Service != row.Source {
					add(callers, u.Service, row.Name)
				}
			}
			for _, d := range st.Services[row.Source].DependsOn {
				if !local[d] {
					add(waiters, d, row.Name)
				}
			}
		case row.Routed:
			for _, u := range row.Upstreams {
				if u.Target == Local {
					add(serves, row.Name, u.Service)
				}
			}
		}
	}
	uses := map[string]string{}
	waited := map[string]bool{} // used only as a job something here waits for
	for svc, who := range waiters {
		uses[svc], waited[svc] = list(who)+" here "+verb(who, "waits", "wait")+" for it", true
	}
	for svc, who := range callers {
		uses[svc], waited[svc] = list(who)+" here "+verb(who, "uses", "use")+" it", false
	}
	for svc, who := range serves {
		uses[svc], waited[svc] = "it serves this worktree's "+list(who), false
	}
	var out []Skew
	said := map[string]bool{} // files already reported for a service used directly
	for _, jobs := range []bool{false, true} {
		for _, name := range sortedKeys(uses) {
			s := st.Services[name]
			if s == nil || len(s.Paths) == 0 || waited[name] != jobs {
				continue
			}
			files := allMatches(s.Paths, mainChanged)
			if jobs {
				// A job in main works on main's state. When a service here
				// takes the same files (this worktree's own database applies
				// its own migrations), main's run of the job doesn't reach
				// it. And when the service the job works on is already
				// listed for those files, saying it twice helps nobody.
				var left []string
				for _, f := range notTakenHere(st, local, files) {
					if !said[f] {
						left = append(left, f)
					}
				}
				files = left
			}
			for _, f := range files {
				said[f] = true
			}
			if len(files) > 0 {
				out = append(out, Skew{Service: name, Why: uses[name], Files: files})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// list joins names as prose: "api", "api and worker", "api, web and worker".
func list(names []string) string {
	sort.Strings(names)
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func verb(names []string, one, many string) string {
	if len(names) == 1 {
		return one
	}
	return many
}

// notTakenHere drops the files that a service running in this worktree
// lists in its paths.
func notTakenHere(st *config.Stack, local map[string]bool, files []string) []string {
	var out []string
	for _, f := range files {
		taken := false
		for name := range local {
			if _, ok := firstMatch(st.Services[name].Paths, []string{f}); ok {
				taken = true
				break
			}
		}
		if !taken {
			out = append(out, f)
		}
	}
	return out
}

// unclaimed returns the changed files that no service's paths take (a `!`
// exclusion still counts as taking it: someone decided it doesn't matter)
// and the stack's ignore list doesn't cover.
func unclaimed(st *config.Stack, changed []string) []string {
	var positive []string
	for _, s := range st.Services {
		for _, pat := range s.Paths {
			if !strings.HasPrefix(pat, "!") {
				positive = append(positive, pat)
			}
		}
	}
	for _, pat := range append(append([]string{}, st.Hydra.GlobalPaths...), st.Hydra.Ignore...) {
		positive = append(positive, strings.TrimPrefix(pat, "!"))
	}
	var out []string
	for _, f := range changed {
		taken := false
		for _, pat := range positive {
			if glob.Match(pat, f) {
				taken = true
				break
			}
		}
		if !taken {
			out = append(out, f)
		}
	}
	return out
}

func dedupe(xs []string) []string {
	sort.Strings(xs)
	var out []string
	for i, x := range xs {
		if i == 0 || x != xs[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// allMatches returns every path that firstMatch would accept.
func allMatches(patterns, paths []string) []string {
	var out []string
	for _, f := range paths {
		if _, ok := firstMatch(patterns, []string{f}); ok {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// prepareTemplate is the service's prepare command, or the one its kind
// supplies when it has none.
func prepareTemplate(st *config.Stack, s *config.Service) string {
	if s.Prepare != "" || s.Kind != config.KindTemporal {
		return s.Prepare
	}
	exe := st.Hydra.Exe
	if exe == "" {
		exe = "hydra"
	}
	return quote(exe) + " prepare temporal --namespace {value} --from " + quote(Value(st, s, 0, ""))
}

func quote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./{}", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
