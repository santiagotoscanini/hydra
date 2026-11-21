// Package config reads a process-compose file extended with `x-hydra` fields.
//
// process-compose ignores keys it doesn't know, so the same file still runs
// under plain `process-compose up`. Hydra renders a clean file per checkout
// anyway, with the `x-hydra` keys stripped.
package config

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Stack is the parsed file: Hydra's view plus the raw document, which the
// renderer copies through untouched.
type Stack struct {
	Hydra    Global
	Services map[string]*Service
	Raw      map[string]any
}

// Global is the top-level `x-hydra` block.
type Global struct {
	// BaseRef is what a worktree is diffed against (default "main").
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
}

// Upstream says which service an env var points at and how to format it.
type Upstream struct {
	Service string `yaml:"service"`
	// Value overrides the target's URL template for this env var, e.g.
	// "{port}" when the consumer wants only the port. Placeholders as in
	// Service.URL.
	Value string `yaml:"value"`
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
	// Follow services run in a worktree whenever a local service depends
	// on them, such as `pnpm install` for that worktree's node_modules.
	Follow bool `yaml:"follow"`
	// Prepare is a shell command run once before the service starts in a
	// worktree, with the same placeholders as URL plus {value} (this
	// service's rendered URL). Used to create a Temporal namespace.
	Prepare string `yaml:"prepare"`

	// DependsOn lists the process names this process waits for, from the
	// regular process-compose `depends_on`.
	DependsOn []string `yaml:"-"`
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

	st := &Stack{Hydra: typed.Hydra, Services: map[string]*Service{}, Raw: raw}
	g := &st.Hydra
	if g.BaseRef == "" {
		g.BaseRef = "main"
	}
	if g.SlotStride == 0 {
		g.SlotStride = 1000
	}
	if g.MaxSlots == 0 {
		g.MaxSlots = 20
	}
	if g.Host == "" {
		g.Host = "127.0.0.1"
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
		for _, env := range s.UpstreamEnvs() {
			u := s.Upstreams[env]
			t, ok := st.Services[u.Service]
			if !ok {
				return fmt.Errorf("%s: upstream %s points at unknown service %q", name, env, u.Service)
			}
			tmpl := u.Value
			if tmpl == "" {
				tmpl = t.URL
			}
			if strings.Contains(tmpl, "{port}") && t.Port == 0 {
				return fmt.Errorf("%s: upstream %s needs %s's port, but %s has no x-hydra.port", name, env, t.Name, t.Name)
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
			p := s.Port + slot*st.Hydra.SlotStride
			who := fmt.Sprintf("%s in slot %d", name, slot)
			if p > 65535 {
				return fmt.Errorf("%s would listen on %d; lower max_slots or slot_stride", who, p)
			}
			if prev, taken := owner[p]; taken {
				return fmt.Errorf("port %d: %s collides with %s; change slot_stride, max_slots or the base ports", p, who, prev)
			}
			owner[p] = who
		}
	}
	return nil
}
