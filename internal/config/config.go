// Package config reads a process-compose file extended with `x-hydra` fields.
//
// process-compose ignores keys it doesn't know, so the same file still runs
// under plain `process-compose up`. Hydra renders a clean file per checkout
// anyway, with the `x-hydra` keys stripped.
package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Stack is the parsed file: Hydra's view plus the raw document, which the
// renderer copies through untouched.
type Stack struct {
	Hydra    Global
	Services map[string]*Service
	Raw      map[string]any
	Source   []byte // the file as it was read
}

// Global is the top-level `x-hydra` block.
type Global struct {
	// BaseRef is what a worktree is diffed against. Empty means the
	// repository's default branch, which the caller works out from git.
	BaseRef string `yaml:"base_ref"`
	// SlotStride is the port offset per worktree slot: slot N listens on
	// port + N*SlotStride (default 1000). Slot 0 is the baseline.
	SlotStride int `yaml:"slot_stride"`
	// MaxSlots caps how many worktrees can run at once (default 20). Port
	// collisions are checked for every slot up to it.
	MaxSlots int `yaml:"max_slots"`
	// ReservedPorts are ports used by shared infra or anything else on the
	// machine; no slot may land on them.
	ReservedPorts []int `yaml:"reserved_ports"`
	// Host used to build upstream URLs (default 127.0.0.1).
	Host string `yaml:"host"`
	// PortEnv is the env var a service reads its port from (default PORT).
	PortEnv string `yaml:"port_env"`
	// GlobalPaths are files that affect every non-shared service.
	GlobalPaths []string `yaml:"global_paths"`
	// Ignore lists files that belong to no service (docs, CI, other apps).
	// A changed file that matches neither a service's paths nor these is
	// reported, so a path someone forgot to list doesn't go unnoticed.
	Ignore []string `yaml:"ignore"`
	// TLS names the certificate hydra's TLS fronts present, for a stack that
	// brings its own (from mkcert, say). Left out, hydra makes a local CA
	// and a certificate signed by it.
	TLS TLSFiles `yaml:"tls"`
	// IdleStop stops a worktree's stack once it has gone this long without
	// activity: a request through hydra's router for it, or a file in it
	// that changed. The main checkout's never stops. 0 (unset, `0` or
	// `off`) never stops one.
	IdleStop Idle `yaml:"idle_stop"`
	// BehindWarn is how many commits behind main a worktree may fall before
	// `hydra status` warns that it is far behind (default 100). Merging main
	// into it conflicting warns at any count.
	BehindWarn int `yaml:"behind_warn"`

	// Exe is the hydra binary that generated steps call (a kind's prepare
	// step). Set by the caller; "hydra" when empty.
	Exe string `yaml:"-"`
}

// Idle is a duration written the way Go writes one ("2h", "90m", "30s"),
// or `0` / `off` for none.
type Idle time.Duration

// UnmarshalYAML accepts "2h", "0" and "off".
func (d *Idle) UnmarshalYAML(n *yaml.Node) error {
	switch v := strings.TrimSpace(n.Value); v {
	case "", "0", "off", "false":
		*d = 0
		return nil
	default:
		dur, err := time.ParseDuration(v)
		if err != nil || dur < 0 {
			return fmt.Errorf("idle_stop: %q is not a duration (2h, 90m, 30s) or off", n.Value)
		}
		*d = Idle(dur)
		return nil
	}
}

// Kinds hydra knows how to give each worktree a copy of.
const (
	// KindTemporal is a Temporal worker. Each worktree's copy polls a
	// namespace of its own (hydra-<slot>; the baseline keeps "default"),
	// created before the worker starts with the baseline namespace's search
	// attributes. Upstreams pointing at it receive the namespace, and on a
	// routed service they switch per request.
	KindTemporal = "temporal"
	// KindSupabase is a local Supabase. The baseline runs the command as
	// written; a worktree's copy is a Supabase CLI stack of its own (stack
	// mode, one database per worktree), on that slot's ports, with its
	// migrations applied after every start.
	KindSupabase = "supabase"
)

var kinds = []string{KindTemporal, KindSupabase}

// SupabasePorts are the Supabase CLI's port settings, as offsets from the
// API port, that a worktree's stack listens on: the env var that sets each
// one, and where it lands. "db" follows Service.Ports["db"] when that is
// set. The rest only have to stay out of every other slot's way.
var SupabasePorts = []struct {
	Env    string
	Name   string // a key of Service.Ports, or "" when hydra picks it alone
	Offset int
}{
	{"SUPABASE_API_PORT", "", 0},
	{"SUPABASE_DB_PORT", "db", 1},
	{"SUPABASE_STUDIO_PORT", "", 2},
	{"SUPABASE_LOCAL_SMTP_PORT", "", 3},
	{"SUPABASE_LOCAL_SMTP_SMTP_PORT", "", 4},
	{"SUPABASE_LOCAL_SMTP_POP3_PORT", "", 5},
	{"SUPABASE_ANALYTICS_PORT", "", 6},
	{"SUPABASE_ANALYTICS_VECTOR_PORT", "", 7},
	{"SUPABASE_DB_POOLER_PORT", "", 8},
	{"SUPABASE_DB_SHADOW_PORT", "", -1},
	{"SUPABASE_EDGE_RUNTIME_INSPECTOR_PORT", "", -2},
}

// TLSFiles are a certificate, its key, and the CA processes have to trust
// to accept it. Paths are relative to the stack file.
type TLSFiles struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
	CA   string `yaml:"ca"`
}

// TLSFront is a port where hydra's router accepts TLS for the baseline's
// copy of a service and passes the bytes on to its plain port. ALPN is the
// one protocol it offers: "http/1.1" (the default; websockets included) or
// "h2" for a service that speaks HTTP/2 without TLS, such as a gRPC server.
type TLSFront struct {
	Port int    `yaml:"port"`
	ALPN string `yaml:"alpn"`
}

// Pool is a service's PgBouncer for worktrees (Service.Pool).
type Pool struct {
	// Port is where PgBouncer listens.
	Port int `yaml:"port"`
	// Of names the port it stands in front of: "db" when the service has a
	// named port of that name (a Supabase stack's Postgres), else its port.
	Of string `yaml:"of"`
	// Size is how many connections PgBouncer keeps to Postgres, for every
	// worktree together (default 20).
	Size int `yaml:"size"`
}

// UnmarshalYAML accepts `pool: 6432` or `pool: { port: 6432, size: 40 }`.
func (p *Pool) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&p.Port)
	}
	type plain Pool
	return n.Decode((*plain)(p))
}

// Pooled returns the service whose Postgres worktrees reach through a pool,
// nil when none has one.
func (st *Stack) Pooled() *Service {
	for _, name := range st.Names() {
		if s := st.Services[name]; s.Pool != nil {
			return s
		}
	}
	return nil
}

// PoolTarget is the port of s, in the baseline, that its pool stands in
// front of.
func (st *Stack) PoolTarget(s *Service) int {
	if s.Pool.Of != "" {
		return s.Ports[s.Pool.Of]
	}
	return s.Port
}

// UnmarshalYAML accepts `tls: 8443` or `tls: { port: 8443, alpn: h2 }`.
func (f *TLSFront) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&f.Port)
	}
	type plain TLSFront
	return n.Decode((*plain)(f))
}

// Upstream says which service an env var points at and how to format it.
type Upstream struct {
	Service string `yaml:"service"`
	// Value overrides the target's URL template for this env var, e.g.
	// "{port}" when the consumer wants only the port. Placeholders as in
	// Service.URL.
	Value string `yaml:"value"`
	// BaselineValue replaces Value for the main checkout's copy alone, where
	// it may use {tls}: the target's TLS front. For a baseline process that
	// runs differently from the worktrees' (a production build that only
	// accepts https), while they keep the plain address.
	BaselineValue string `yaml:"baseline_value"`
	// PerRequest says a routed service picks this value per request itself,
	// from the value hydra's hook hands it, instead of once from its
	// environment. For targets a proxy can't route, such as a Temporal
	// namespace.
	PerRequest bool `yaml:"per_request"`
}

// UnmarshalYAML accepts `ENV: service` or `ENV: {service: s, value: "..."}`.
func (u *Upstream) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		u.Service = n.Value
		return nil
	}
	type plain Upstream
	return n.Decode((*plain)(u))
}

// Service is a process's `x-hydra` block.
type Service struct {
	Name string `yaml:"-"`
	// Port the baseline instance listens on.
	Port int `yaml:"port"`
	// Ports are further ports of the same service, by name, shifted per slot
	// like Port. Upstream values reach them as {port.NAME}.
	Ports map[string]int `yaml:"ports"`
	// WorktreePort, when set, is Port in slot 1: slot N listens on
	// WorktreePort + (N-1)*SlotStride instead of Port + N*SlotStride, and its
	// named ports move with it. It keeps a high base port (Supabase's 54321)
	// out of the range macOS hands out to outgoing connections (49152+).
	WorktreePort int `yaml:"worktree_port"`
	// PortEnv overrides Global.PortEnv for this service.
	PortEnv string `yaml:"port_env"`
	// Paths (globs, relative to the repo root) whose changes mean this
	// service must run from the worktree.
	Paths []string `yaml:"paths"`
	// Upstreams maps an env var to the service it should point at. Hydra
	// fills in the value for either the baseline or the worktree instance.
	// A service may point at itself (say, its own public URL).
	Upstreams map[string]Upstream `yaml:"upstreams"`
	// URL is what an upstream env var receives. Placeholders: {host},
	// {port}, {slot}. Default "http://{host}:{port}".
	URL string `yaml:"url"`
	// BaselineURL replaces URL for the baseline instance (slot 0), for
	// values that aren't derived from a port, such as a Temporal namespace.
	BaselineURL string `yaml:"baseline_url"`
	// Shared services always come from the baseline (databases, Docker,
	// tunnels). A change to their paths is reported as a warning.
	Shared bool `yaml:"shared"`
	// Kind marks a service hydra knows more about than its port: "temporal"
	// for a Temporal worker. It fills in url, baseline_url and prepare.
	Kind string `yaml:"kind"`
	// Routed services have one copy, the baseline's, that serves every
	// worktree which didn't change them: each worktree reaches it through a
	// port of its own, and the calls it makes while serving such a request go
	// to that worktree's services. The service has to pass the request's
	// X-Hydra-Worktree header on to the calls it makes: "node" has hydra's
	// preload hook do that, "true" says the service does it itself.
	Routed Routing `yaml:"routed"`
	// Follow services run in a worktree whenever a local service depends
	// on them, such as `pnpm install` for that worktree's node_modules.
	Follow bool `yaml:"follow"`
	// Prepare is a shell command run once before the service starts in a
	// worktree, with the same placeholders as URL plus {value} (this
	// service's rendered URL). Used to create a Temporal namespace.
	Prepare string `yaml:"prepare"`
	// BaselineCommand replaces the process's command in the main checkout,
	// where the code only changes on `hydra update`: a compiled binary or a
	// production build instead of a dev server that watches files.
	BaselineCommand string `yaml:"baseline_command"`
	// TLS puts a TLS front before the baseline's copy (see TLSFront).
	TLS *TLSFront `yaml:"tls"`
	// Hosts is a port where hydra's router serves each worktree's copy of
	// the service by name, <worktree>.localhost:Hosts, and main's at
	// localhost:Hosts. A worktree that doesn't run the service reaches what
	// it uses instead: main's copy, through its own port when routed.
	Hosts int `yaml:"hosts"`
	// Pool puts one PgBouncer in front of the baseline's Postgres, through
	// which every worktree that uses the baseline's copy connects, so they
	// share a few connections instead of each opening its own pools. The
	// baseline itself, and a worktree that runs its own copy, connect
	// directly.
	Pool *Pool `yaml:"pool"`
	// SeedFromMain gives a worktree's own Supabase main's data instead of
	// the seed files: when the stack is created (and on `hydra db reseed`),
	// hydra applies the migrations main's database has, copies main's rows
	// in, and only then applies the migrations that are the branch's own.
	SeedFromMain bool `yaml:"seed_from_main"`

	// DependsOn lists the process names this process waits for, from the
	// regular process-compose `depends_on`.
	DependsOn []string `yaml:"-"`
}

// Routing is how a routed service passes the worktree tag along.
type Routing string

const (
	NotRouted  Routing = ""
	RoutedSelf Routing = "true" // the service forwards the header itself
	RoutedNode Routing = "node" // hydra's Node preload hook forwards it
)

// UnmarshalYAML accepts `routed: true`, `routed: false` and `routed: node`.
func (r *Routing) UnmarshalYAML(n *yaml.Node) error {
	switch n.Value {
	case "false", "":
		*r = NotRouted
	case "true":
		*r = RoutedSelf
	case "node":
		*r = RoutedNode
	default:
		return fmt.Errorf("line %d: routed: want true or node, got %q", n.Line, n.Value)
	}
	return nil
}

// namedPort matches a {port.NAME} placeholder.
var namedPort = regexp.MustCompile(`\{port\.([A-Za-z0-9_-]+)\}`)

// Routable reports whether a proxy can route calls made through this upstream:
// its value has to carry the target's port, as an HTTP URL or as the bare
// port. A postgres:// URL or a queue name isn't.
func (st *Stack) Routable(u Upstream) bool {
	tmpl := u.Value
	if tmpl == "" {
		tmpl = st.Services[u.Service].URL
	}
	if !strings.Contains(tmpl, "{port}") {
		return false
	}
	if scheme, _, ok := strings.Cut(tmpl, "://"); ok {
		switch scheme {
		case "http", "https", "ws", "wss":
		default:
			return false
		}
	}
	return true
}

// Dispatches reports whether routed service s reaches u's target through
// the router. Not when s also points at that service through a value no
// proxy can route, such as a database URL: a worktree running that service
// runs its own copy of s too, so main's copy only ever serves worktrees that
// use main's, and its calls can go there directly.
func (st *Stack) Dispatches(s *Service, u Upstream) bool {
	if s.Routed == NotRouted || u.Service == s.Name || !st.Routable(u) || u.BaselineValue != "" {
		return false
	}
	for _, o := range s.Upstreams {
		if o.Service == u.Service && !o.PerRequest && !st.Routable(o) {
			return false
		}
	}
	return true
}

// DispatchPort is where the router listens for calls to s made by routed
// services: one stride past the last slot.
func (st *Stack) DispatchPort(s *Service) int {
	return s.Port + (st.Hydra.MaxSlots+1)*st.Hydra.SlotStride
}

// Dispatched returns the services routed services call through the router,
// sorted.
func (st *Stack) Dispatched() []string {
	seen := map[string]bool{}
	for _, s := range st.Services {
		if s.Routed == NotRouted {
			continue
		}
		for _, u := range s.Upstreams {
			if st.Dispatches(s, u) {
				seen[u.Service] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Shift is how far s's ports in slot are from its baseline ports.
func (st *Stack) Shift(s *Service, slot int) int {
	if slot > 0 && s.WorktreePort != 0 && s.Port != 0 {
		return s.WorktreePort - s.Port + (slot-1)*st.Hydra.SlotStride
	}
	return slot * st.Hydra.SlotStride
}

// badPorts are the ports browsers refuse to connect to (the Fetch
// standard's "bad ports"), which Next.js also refuses to listen on.
var badPorts = map[int]bool{
	1: true, 7: true, 9: true, 11: true, 13: true, 15: true, 17: true, 19: true, 20: true,
	21: true, 22: true, 23: true, 25: true, 37: true, 42: true, 43: true, 53: true, 69: true,
	77: true, 79: true, 87: true, 95: true, 101: true, 102: true, 103: true, 104: true,
	109: true, 110: true, 111: true, 113: true, 115: true, 117: true, 119: true, 123: true,
	135: true, 137: true, 139: true, 143: true, 161: true, 179: true, 389: true, 427: true,
	465: true, 512: true, 513: true, 514: true, 515: true, 526: true, 530: true, 531: true,
	532: true, 540: true, 548: true, 554: true, 556: true, 563: true, 587: true, 601: true,
	636: true, 989: true, 990: true, 993: true, 995: true, 1719: true, 1720: true, 1723: true,
	2049: true, 3659: true, 4045: true, 4190: true, 5060: true, 5061: true, 6000: true,
	6566: true, 6665: true, 6666: true, 6667: true, 6668: true, 6669: true, 6679: true,
	6697: true, 10080: true,
}

// BadSlot names a port of slot that a browser would refuse, "" when there is
// none. Such a slot is never handed out: a page on it can't be opened, and
// Next.js won't even start there.
func (st *Stack) BadSlot(slot int) string {
	for _, name := range st.Names() {
		s := st.Services[name]
		if s.Shared {
			continue
		}
		ports := st.SlotPorts(s, slot)
		labels := make([]string, 0, len(ports))
		for l := range ports {
			labels = append(labels, l)
		}
		sort.Strings(labels)
		for _, l := range labels {
			if badPorts[ports[l]] {
				if l == "" {
					return fmt.Sprintf("%s on %d", name, ports[l])
				}
				return fmt.Sprintf("%s's %s port on %d", name, l, ports[l])
			}
		}
	}
	return ""
}

// SlotPorts lists every port s listens on in a slot, keyed by name ("" for
// Port): its named ports, and in a worktree the rest of a Supabase stack's.
func (st *Stack) SlotPorts(s *Service, slot int) map[string]int {
	if s.Port == 0 {
		return nil
	}
	shift := st.Shift(s, slot)
	out := map[string]int{"": s.Port + shift}
	for name, p := range s.Ports {
		out[name] = p + shift
	}
	if s.Kind == KindSupabase && slot > 0 {
		for _, sp := range SupabasePorts {
			if sp.Name == "" && sp.Offset != 0 {
				out[strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(sp.Env, "SUPABASE_"), "_PORT"))] = s.Port + sp.Offset + shift
			}
		}
	}
	return out
}

// SupabaseEnv is the environment that makes the Supabase CLI run a
// worktree's own stack: stack mode on, every port on the slot's, and the Deno
// version stack mode supports (it rejects any other).
func (st *Stack) SupabaseEnv(s *Service, slot int) []string {
	env := []string{"SUPABASE_EXPERIMENTAL_STACK=1", "SUPABASE_EDGE_RUNTIME_DENO_VERSION=2"}
	shift := st.Shift(s, slot)
	for _, sp := range SupabasePorts {
		p := s.Port + sp.Offset
		if sp.Name != "" {
			p = s.Ports[sp.Name]
		}
		env = append(env, fmt.Sprintf("%s=%d", sp.Env, p+shift))
	}
	return env
}

// HasTLS reports whether any service has a TLS front.
func (st *Stack) HasTLS() bool {
	for _, s := range st.Services {
		if s.TLS != nil {
			return true
		}
	}
	return false
}

// HasHosts reports whether any service has a hosts port.
func (st *Stack) HasHosts() bool {
	for _, s := range st.Services {
		if s.Hosts != 0 {
			return true
		}
	}
	return false
}

// HostsURL is the address of s's copy in the checkout whose name is label
// ("" for main's): http://<label>.localhost:<hosts>, http://localhost:<hosts>
// for main.
func (st *Stack) HostsURL(s *Service, label string) string {
	if s == nil || s.Hosts == 0 {
		return ""
	}
	host := "localhost"
	if label != "" {
		host = label + ".localhost"
	}
	return fmt.Sprintf("http://%s:%d", host, s.Hosts)
}

// HostLabel is a worktree's name under .localhost: its folder's name,
// lowercased, with anything a host name can't hold made a hyphen, and cut
// to the 63 characters a DNS label holds. "" when nothing is left.
func HostLabel(root string) string {
	base := root
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	label := strings.Trim(b.String(), "-")
	if len(label) > 63 {
		label = strings.TrimRight(label[:63], "-")
	}
	return label
}

// HasRouted reports whether any service is routed.
func (st *Stack) HasRouted() bool {
	for _, s := range st.Services {
		if s.Routed != NotRouted {
			return true
		}
	}
	return false
}

// UpstreamEnvs returns the upstream env var names in a stable order.
func (s *Service) UpstreamEnvs() []string {
	keys := make([]string, 0, len(s.Upstreams))
	for k := range s.Upstreams {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Names returns service names sorted.
func (st *Stack) Names() []string {
	names := make([]string, 0, len(st.Services))
	for n := range st.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

type typedFile struct {
	Hydra     Global `yaml:"x-hydra"`
	Processes map[string]struct {
		Hydra     *Service       `yaml:"x-hydra"`
		DependsOn map[string]any `yaml:"depends_on"`
	} `yaml:"processes"`
}

// HasHydra reports whether a file carries any x-hydra configuration.
func HasHydra(data []byte) bool {
	var probe struct {
		Hydra map[string]any `yaml:"x-hydra"`
	}
	return yaml.Unmarshal(data, &probe) == nil && probe.Hydra != nil
}

// Load reads and validates a stack file.
func Load(path string) (*Stack, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	st, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return st, nil
}

// Parse decodes and validates a stack file's contents.
func Parse(data []byte) (*Stack, error) {
	var typed typedFile
	if err := yaml.Unmarshal(data, &typed); err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	st := &Stack{Hydra: typed.Hydra, Services: map[string]*Service{}, Raw: raw, Source: data}
	g := &st.Hydra
	if g.SlotStride == 0 {
		g.SlotStride = 1000
	}
	if g.MaxSlots == 0 {
		g.MaxSlots = 20
	}
	if g.Host == "" {
		g.Host = "127.0.0.1"
	}
	if g.BehindWarn == 0 {
		g.BehindWarn = 100
	}
	if g.BehindWarn < 0 {
		return nil, fmt.Errorf("behind_warn: %d is not a number of commits", g.BehindWarn)
	}
	if g.PortEnv == "" {
		g.PortEnv = "PORT"
	}

	for name, p := range typed.Processes {
		s := p.Hydra
		if s == nil {
			// No x-hydra: a plain process that only runs in the baseline.
			s = &Service{Shared: true}
		}
		s.Name = name
		if s.Kind == KindTemporal {
			if s.URL == "" {
				s.URL = "hydra-{slot}"
			}
			if s.BaselineURL == "" {
				s.BaselineURL = "default"
			}
		}
		if s.Kind == KindSupabase {
			if s.Port == 0 {
				s.Port = 54321
			}
			if s.Ports == nil {
				s.Ports = map[string]int{}
			}
			if s.Ports["db"] == 0 {
				s.Ports["db"] = s.Port + 1
			}
		}
		if s.TLS != nil && s.TLS.ALPN == "" {
			s.TLS.ALPN = "http/1.1"
		}
		if s.Pool != nil {
			if s.Pool.Of == "" && s.Ports["db"] != 0 {
				s.Pool.Of = "db"
			}
			if s.Pool.Size == 0 {
				s.Pool.Size = 20
			}
		}
		if s.URL == "" {
			s.URL = "http://{host}:{port}"
		}
		if s.PortEnv == "" {
			s.PortEnv = g.PortEnv
		}
		for d := range p.DependsOn {
			s.DependsOn = append(s.DependsOn, d)
		}
		sort.Strings(s.DependsOn)
		st.Services[name] = s
	}

	// A routed service can't take a Temporal namespace from its environment
	// (main's copy has main's), so it gets it per request.
	for _, s := range st.Services {
		if s.Routed == NotRouted {
			continue
		}
		for env, u := range s.Upstreams {
			if t := st.Services[u.Service]; t != nil && t.Kind == KindTemporal {
				u.PerRequest = true
				s.Upstreams[env] = u
			}
		}
	}

	if err := st.validate(); err != nil {
		return nil, err
	}
	return st, nil
}

func (st *Stack) validate() error {
	for _, name := range st.Names() {
		s := st.Services[name]
		if s.Shared && s.Follow {
			return fmt.Errorf("%s: shared and follow can't both be set", name)
		}
		if s.Kind != "" {
			known := false
			for _, k := range kinds {
				known = known || s.Kind == k
			}
			switch {
			case !known:
				return fmt.Errorf("%s: unknown kind %q (known: %s)", name, s.Kind, strings.Join(kinds, ", "))
			case s.Shared || s.Follow || s.Routed != NotRouted:
				return fmt.Errorf("%s: kind %s gives each worktree its own copy; it can't be shared, follow or routed", name, s.Kind)
			}
		}
		if s.SeedFromMain && s.Kind != KindSupabase {
			return fmt.Errorf("%s: seed_from_main is for kind: supabase, a database each worktree gets a copy of", name)
		}
		if s.Routed != NotRouted {
			switch {
			case s.Shared || s.Follow:
				return fmt.Errorf("%s: routed can't be combined with shared or follow", name)
			case s.Port == 0:
				return fmt.Errorf("%s: routed needs x-hydra.port, the port worktrees reach it through", name)
			}
		}
		if s.Pool != nil {
			switch {
			case s.Pool.Port == 0:
				return fmt.Errorf("%s: pool needs a port of its own for PgBouncer: `pool: 6432` or `pool: { port: 6432 }`", name)
			case s.Pool.Of != "" && s.Ports[s.Pool.Of] == 0:
				return fmt.Errorf("%s: pool.of: %s has no port named %q", name, name, s.Pool.Of)
			case s.Pool.Of == "" && s.Port == 0:
				return fmt.Errorf("%s: pool needs x-hydra.port, the Postgres port it stands in front of", name)
			case st.Pooled() != s:
				return fmt.Errorf("%s: pool: only one service can have one, and %s has it already", name, st.Pooled().Name)
			}
		}
		if s.TLS != nil {
			switch {
			case s.Port == 0:
				return fmt.Errorf("%s: tls needs x-hydra.port, the port the front passes connections to", name)
			case s.TLS.Port == 0:
				return fmt.Errorf("%s: tls needs a port of its own: `tls: 8443` or `tls: { port: 8443 }`", name)
			case s.TLS.ALPN != "http/1.1" && s.TLS.ALPN != "h2":
				return fmt.Errorf("%s: tls.alpn: want http/1.1 or h2, got %q", name, s.TLS.ALPN)
			}
		}
		if s.Hosts != 0 {
			switch {
			case s.Port == 0:
				return fmt.Errorf("%s: hosts needs x-hydra.port, the port each copy listens on", name)
			case s.Shared:
				return fmt.Errorf("%s: hosts gives each worktree's copy an address, and a shared service only has main's", name)
			case !strings.HasPrefix(s.URL, "http"):
				return fmt.Errorf("%s: hosts serves HTTP by name, and %s's url is %q", name, name, s.URL)
			}
		}
		for _, env := range s.UpstreamEnvs() {
			u := s.Upstreams[env]
			t, ok := st.Services[u.Service]
			if !ok {
				return fmt.Errorf("%s: upstream %s points at unknown service %q", name, env, u.Service)
			}
			if strings.Contains(u.Value, "{tls}") || strings.Contains(t.URL, "{tls}") {
				return fmt.Errorf("%s: upstream %s: {tls} is the main checkout's TLS front, so it belongs in baseline_value; a worktree's own %s has none", name, env, t.Name)
			}
			if strings.Contains(u.BaselineValue, "{tls}") && t.TLS == nil {
				return fmt.Errorf("%s: upstream %s uses {tls}, but %s has no x-hydra.tls", name, env, t.Name)
			}
			tmpl := u.Value
			if tmpl == "" {
				tmpl = t.URL
			}
			if strings.Contains(tmpl, "{port}") && t.Port == 0 {
				return fmt.Errorf("%s: upstream %s needs %s's port, but %s has no x-hydra.port", name, env, t.Name, t.Name)
			}
			for _, m := range namedPort.FindAllStringSubmatch(tmpl, -1) {
				if _, ok := t.Ports[m[1]]; !ok {
					return fmt.Errorf("%s: upstream %s uses {port.%s}, but %s has no x-hydra.ports.%s", name, env, m[1], t.Name, m[1])
				}
			}
			if u.PerRequest && s.Routed == NotRouted {
				return fmt.Errorf("%s: upstream %s is per_request, which only applies to a routed service", name, env)
			}
		}
	}
	return st.checkPorts()
}

// checkPorts makes sure no two instances in any slot up to MaxSlots share a
// port, and none lands on a reserved port.
func (st *Stack) checkPorts() error {
	owner := map[int]string{}
	for _, p := range st.Hydra.ReservedPorts {
		owner[p] = "reserved"
	}
	for slot := 0; slot <= st.Hydra.MaxSlots; slot++ {
		for _, name := range st.Names() {
			s := st.Services[name]
			if s.Port == 0 || (slot > 0 && s.Shared) {
				continue
			}
			ports := st.SlotPorts(s, slot)
			labels := make([]string, 0, len(ports))
			for label := range ports {
				labels = append(labels, label)
			}
			sort.Strings(labels)
			for _, label := range labels {
				p := ports[label]
				who := fmt.Sprintf("%s in slot %d", name, slot)
				if label != "" {
					who = fmt.Sprintf("%s's %s port in slot %d", name, label, slot)
				}
				if p > 65535 {
					return fmt.Errorf("%s would listen on %d; lower max_slots or slot_stride", who, p)
				}
				if s.Kind == KindSupabase && slot > 0 && p >= 10000 && p < 20000 {
					return fmt.Errorf("%s would be %d, which the Supabase CLI keeps for its own services (10000-19999); move x-hydra.port", who, p)
				}
				if prev, taken := owner[p]; taken {
					return fmt.Errorf("port %d: %s collides with %s; change slot_stride, max_slots or the base ports", p, who, prev)
				}
				owner[p] = who
			}
		}
	}
	for _, name := range st.Names() {
		s := st.Services[name]
		if s.TLS == nil {
			continue
		}
		who := name + "'s TLS front"
		if prev, taken := owner[s.TLS.Port]; taken {
			return fmt.Errorf("port %d: %s collides with %s; change it, slot_stride, max_slots or the base ports", s.TLS.Port, who, prev)
		}
		owner[s.TLS.Port] = who
	}
	for _, name := range st.Names() {
		s := st.Services[name]
		if s.Hosts == 0 {
			continue
		}
		who := name + "'s hosts port"
		if prev, taken := owner[s.Hosts]; taken {
			return fmt.Errorf("port %d: %s collides with %s; change it, slot_stride, max_slots or the base ports", s.Hosts, who, prev)
		}
		owner[s.Hosts] = who
	}
	if s := st.Pooled(); s != nil {
		who := s.Name + "'s pool (PgBouncer)"
		if prev, taken := owner[s.Pool.Port]; taken {
			return fmt.Errorf("port %d: %s collides with %s; change it, slot_stride, max_slots or the base ports", s.Pool.Port, who, prev)
		}
		owner[s.Pool.Port] = who
	}
	for _, name := range st.Dispatched() {
		p := st.DispatchPort(st.Services[name])
		who := "the router's port for " + name
		if p > 65535 {
			return fmt.Errorf("%s would be %d; lower max_slots or slot_stride", who, p)
		}
		if prev, taken := owner[p]; taken {
			return fmt.Errorf("port %d: %s collides with %s; change slot_stride, max_slots or the base ports", p, who, prev)
		}
		owner[p] = who
	}
	return nil
}
