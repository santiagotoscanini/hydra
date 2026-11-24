// Command hydra wraps process-compose so each git worktree runs only the
// services it changed and points everything else at the baseline (the main
// checkout).
//
//	hydra [process-compose args]   run process-compose with the rendered file
//	hydra plan  [flags]            show what this checkout runs and how it is wired
//	hydra render [flags] [-o FILE] print or write the rendered file
//	hydra slots [free [PATH]]      list worktree slots, or free one
//	hydra status [--all]           check this checkout against main's stack
//	hydra update                   pull main and bring everything along
//	hydra ui                       the same as a local web page
//	hydra trust                    trust the CA behind TLS fronts
//	hydra version                  which hydra, which process-compose
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"gopkg.in/yaml.v3"

	"github.com/santiagotoscanini/hydra/internal/changes"
	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/hook"
	"github.com/santiagotoscanini/hydra/internal/plan"
	"github.com/santiagotoscanini/hydra/internal/render"
	"github.com/santiagotoscanini/hydra/internal/slots"
	"github.com/santiagotoscanini/hydra/internal/stack"
)

const usage = `hydra: run only what a worktree changed, and take the rest from main.

Use it in place of process-compose; arguments pass through:
  hydra                    start this checkout (TUI)
  hydra up -D              start it in the background
  hydra up web             also run web here, even if unchanged; on a stack
                           that is already up, it adds web to it
  hydra attach | down | process ...
  hydra down --worktrees   stop every worktree's stack; main's keeps running

In the main checkout everything runs, as plain process-compose would. In a
worktree only the services it changed run, on that worktree's own ports, with
their upstreams pointing at main's copies or at each other. (The config and
the flags call the main checkout's stack the baseline.)

Before it starts anything in a worktree, hydra checks the main checkout:
its stack has to be running, and in step with the branch for every service
the worktree takes from it. hydra up --force starts anyway. Starting a stack
that is already up changes nothing and is not an error.

A worktree that is removed while its stack runs stops that stack by itself,
and the next start in any checkout frees its slot.

Hydra's own commands:
  hydra status [--all] [--wait]  is everything this checkout uses up and in step
                                 with main? (exit 1 if not; --all: every worktree;
                                 --wait: first wait for what is still starting,
                                 for at most --timeout D, 10m by default; what is
                                 still starting then is an error)
  hydra update [flags]           pull main, restart what the new commits touched
                                 in its stack, merge main into the worktrees
  hydra ui [--port N] [--no-open]  a local web page of all of it: main, every
                                 worktree, what each runs and takes from main,
                                 commits, memory (default port 4937)
  hydra trust [--remove|--print] make this machine trust the local CA behind
                                 hydra's TLS fronts (x-hydra.tls on a service)
  hydra plan   [flags]           show what this checkout runs and how it is wired
  hydra render [flags] [-o FILE] print (or write) the rendered process-compose file
  hydra slots                    list worktree slots
  hydra slots free [PATH]        stop a worktree's stack and free its slot
                                 (default: this worktree)
  hydra version                  which hydra this is, and which process-compose

Flags for update:
  -n, --dry-run    say what would happen, change nothing
  --main-only      leave the worktrees alone
  --no-pull        don't pull; bring the stack and the worktrees to where main is
  --timeout D      how long one process may take to come back (default 15m)

Flags for plan and render:
  -f FILE          stack file (default <checkout>/process-compose.yaml)
  --root DIR       checkout to plan for (default: the one containing the cwd)
  --slot N         pretend to be worktree slot N (0 = the main checkout)
  --changed PATHS  comma-separated changed paths instead of asking git
  --with SVC       also run SVC here (repeatable, comma-separated ok)
  --without SVC    never run SVC here (repeatable)
  --route S.U=T    point service S's upstream U at T = baseline|local
                   (main's copy, or the one here)
  --unclaimed      (plan) only list the changed files no service's paths cover

A git-ignored .hydra.local.yaml in the checkout adds the same settings:
  with: [web]
  without: [worker]
  routes:
    web: { api: baseline }

Docs: https://hydra.toscanini.me/docs/
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		if code, ok := err.(errExit); ok {
			os.Exit(int(code))
		}
		if _, said := err.(errProblems); !said {
			fmt.Fprintln(os.Stderr, "hydra:", err)
		}
		os.Exit(1)
	}
}

// errExit ends the command with a status that isn't hydra's to explain: the
// one of a command it ran, which already said what went wrong.
type errExit int

func (e errExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "help", "-h", "--help":
			fmt.Print(usage)
			return nil
		case "version", "--version":
			return versionCmd()
		case "plan", "render":
			return planOrRender(args[0], args[1:])
		case "slots":
			return slotsCmd(args[1:])
		case "status":
			return statusCmd(args[1:])
		case "update":
			return updateCmd(args[1:])
		case "ui":
			return uiCmd(args[1:])
		case "trust":
			return trustCmd(args[1:])
		case "watch":
			return watchCmd(args[1:])
		case "router":
			return routerCmd(args[1:])
		case "prepare":
			return prepareCmd(args[1:])
		}
	}
	return wrap(args)
}

type multi []string

func (m *multi) String() string { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*m = append(*m, p)
		}
	}
	return nil
}

// target is everything needed to plan one checkout.
type target struct {
	checkout gitx.Checkout
	file     string
	stack    *config.Stack
}

func resolve(root, file string) (*target, error) {
	if root == "" {
		root = "."
	}
	co, err := gitx.Find(root)
	if err != nil {
		return nil, err
	}
	if file == "" {
		file = defaultFile(co)
	}
	st, err := config.Load(file)
	if err != nil {
		return nil, err
	}
	if st.Hydra.BaseRef == "" {
		st.Hydra.BaseRef = gitx.DefaultBranch(co)
	}
	if st.Hydra.Exe, err = stableExecutable(); err != nil {
		return nil, err
	}
	return &target{checkout: co, file: file, stack: st}, nil
}

// defaultFile is the checkout's process-compose.yaml, or the main
// checkout's when a worktree has none of its own (the file is often personal
// and git-excluded, so a fresh worktree lacks it).
func defaultFile(co gitx.Checkout) string {
	own := filepath.Join(co.Root, "process-compose.yaml")
	if _, err := os.Stat(own); err == nil || co.IsMain() {
		return own
	}
	return filepath.Join(co.Main, "process-compose.yaml")
}

// options are the knobs shared by plan, render and the wrapper.
type options struct {
	slot    int // -1: decide from the checkout
	changed []string
	with    []string
	without []string
	routes  []string
}

// build plans the checkout. The main checkout gets the baseline plan unless
// a slot is forced.
func (t *target) build(o options) (*plan.Plan, error) {
	slot := o.slot
	if slot < 0 {
		if t.checkout.IsMain() {
			slot = 0
		} else {
			s, err := slots.Acquire(t.checkout.CommonDir, t.checkout.Root, t.stack.Hydra.MaxSlots)
			if err != nil {
				return nil, err
			}
			slot = s
		}
	}
	if slot == 0 {
		return plan.BaselinePlan(t.stack), nil
	}

	req := plan.Request{Slot: slot, With: o.with, Without: o.without, Routes: plan.Routes{}}
	lf, err := readLocal(filepath.Join(t.checkout.Root, ".hydra.local.yaml"))
	if err != nil {
		return nil, err
	}
	if lf != nil {
		req.With = append(req.With, lf.With...)
		req.Without = append(req.Without, lf.Without...)
		for svc, ups := range lf.Routes {
			for up, v := range ups {
				tg, err := plan.ParseTarget(v)
				if err != nil {
					return nil, fmt.Errorf(".hydra.local.yaml: %w", err)
				}
				req.Routes.Set(svc, up, tg)
			}
		}
	}
	for _, r := range o.routes { // flags beat the file
		svc, up, tg, err := plan.ParseRoute(r)
		if err != nil {
			return nil, err
		}
		req.Routes.Set(svc, up, tg)
	}

	var warnings []string
	if len(o.changed) > 0 {
		req.Changed = o.changed
	} else {
		var base string
		req.Layers, base, warnings, err = t.layers()
		if err != nil {
			return nil, err
		}
		if req.MainChanged, req.MainBehind, err = t.mainChanged(base); err != nil {
			return nil, err
		}
	}
	p, err := plan.Build(t.stack, req)
	if p != nil {
		p.Warnings = append(warnings, p.Warnings...)
	}
	return p, err
}

// mainChanged lists the files that differ between base, the commit where
// this worktree's branch (or the bottom of its stack) left main, and the
// commit the main checkout has checked out: the code its services run.
// behind is true when main is the older of the two, which happens when a
// worktree is cut from origin's main before the main checkout pulled it.
func (t *target) mainChanged(base string) (files []string, behind bool, err error) {
	head, err := gitx.Head(t.checkout.Main)
	if err != nil {
		return nil, false, fmt.Errorf("reading the main checkout's HEAD: %w", err)
	}
	if base == "" || base == head {
		return nil, false, nil
	}
	files, err = gitx.DiffNames(t.checkout.Main, base, head)
	if err != nil {
		return nil, false, err
	}
	return files, gitx.IsAncestor(t.checkout.Main, head, base), nil
}

// layers finds the stack of worktrees this checkout sits on and what each
// one changed relative to its parent. base is where the bottom one left
// the base ref.
func (t *target) layers() (layers []plan.Layer, base string, warnings []string, err error) {
	chain, warnings, err := stack.Chain(t.checkout.Root, stack.Options{
		BaseRef: t.stack.Hydra.BaseRef,
		ParentOverride: func(root string) string {
			lf, _ := readLocal(filepath.Join(root, ".hydra.local.yaml"))
			if lf == nil {
				return ""
			}
			return lf.Parent
		},
	})
	if err != nil {
		return nil, "", nil, fmt.Errorf("finding the worktree stack: %w", err)
	}
	var out []plan.Layer
	for _, l := range chain {
		changed, err := changes.FromBase(l.Root, l.Base)
		if err != nil {
			return nil, "", nil, fmt.Errorf("detecting changes in %s: %w", l.Root, err)
		}
		out = append(out, plan.Layer{Name: shortBranch(l.Name), Root: l.Root, Changed: changed})
	}
	if len(chain) > 0 {
		base = chain[0].Base
	}
	return out, base, warnings, nil
}

// shortBranch keeps messages readable with long branch names:
// "santiago/fla-710-a-customer-approves-their-share" -> "fla-710-a-customer-appro…".
func shortBranch(b string) string {
	if i := strings.LastIndex(b, "/"); i >= 0 {
		b = b[i+1:]
	}
	if r := []rune(b); len(r) > 26 {
		b = string(r[:25]) + "…"
	}
	return b
}

func (t *target) render(p *plan.Plan, watch string) ([]byte, error) {
	opt := render.Options{Dir: t.checkout.Root, Watch: watch}
	if p.Slot == 0 && (t.stack.HasRouted() || t.stack.HasTLS()) {
		var err error
		if opt.Router, err = routerCommand(t); err != nil {
			return nil, err
		}
	}
	if p.Slot == 0 && t.stack.HasRouted() {
		var err error
		if opt.NodeHook, err = hook.Install(t.checkout.CommonDir); err != nil {
			return nil, err
		}
	}
	if p.Slot == 0 && t.stack.HasTLS() {
		files, err := tlsFilesFor(t)
		if err != nil {
			return nil, err
		}
		opt.CAFile = files.CA
	}
	return render.Render(t.stack, p, opt)
}

func planOrRender(cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	file := fs.String("f", "", "")
	root := fs.String("root", "", "")
	out := fs.String("o", "", "")
	slot := fs.Int("slot", -1, "")
	listUnclaimed := fs.Bool("unclaimed", false, "")
	var o options
	var changed, with, without, routes multi
	fs.Var(&changed, "changed", "")
	fs.Var(&with, "with", "")
	fs.Var(&without, "without", "")
	fs.Var(&routes, "route", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	o = options{slot: *slot, changed: changed, with: with, without: without, routes: routes}

	t, err := resolve(*root, *file)
	if err != nil {
		return err
	}
	p, err := t.build(o)
	if err != nil {
		return err
	}
	if cmd == "plan" && *listUnclaimed {
		for _, f := range p.Unclaimed {
			fmt.Println(f)
		}
		return nil
	}
	if cmd == "plan" {
		printPlan(t, p)
		return nil
	}
	data, err := t.render(p, "")
	if err != nil {
		return err
	}
	if *out == "" {
		_, err = os.Stdout.Write(data)
		return err
	}
	return writeFile(*out, data)
}

func slotsCmd(args []string) error {
	co, err := gitx.Find(".")
	if err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "free" {
		root := co.Root
		if len(args) > 1 {
			if root, err = filepath.Abs(args[1]); err != nil {
				return err
			}
		}
		for _, e := range mustList(co.CommonDir) {
			if e.Root != root {
				continue
			}
			// Freeing a slot hands its ports on, so first stop what holds
			// them. A worktree that is still there keeps its database
			// (stopped); a removed one loses it.
			did, err := reap(co, e)
			if err != nil {
				return fmt.Errorf("slot %d stays taken: %w", e.Slot, err)
			}
			fmt.Printf("slot %d is free%s\n", e.Slot, andDid(did))
			return nil
		}
		return fmt.Errorf("%s holds no slot (`hydra slots` lists them)", root)
	}
	entries, err := slots.List(co.CommonDir)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("no worktree slots taken")
	}
	for _, e := range entries {
		mark, note := " ", ""
		if e.Root == co.Root {
			mark = "*"
		}
		if e.Missing() {
			note = "  (removed; the next `hydra up` cleans up after it, or now: hydra slots free " + shQuote(e.Root) + ")"
		}
		fmt.Printf("%s %2d  %s%s\n", mark, e.Slot, e.Root, note)
	}
	return nil
}

func andDid(did []string) string {
	if len(did) == 0 {
		return ""
	}
	return " (" + strings.Join(did, ", ") + ")"
}

func mustList(commonDir string) []slots.Entry {
	entries, _ := slots.List(commonDir)
	return entries
}

type localFile struct {
	// Parent is the branch this worktree is stacked on, when git ancestry
	// can't tell (or tells wrong). The base ref (main) means "not stacked".
	Parent  string                       `yaml:"parent"`
	With    []string                     `yaml:"with"`
	Without []string                     `yaml:"without"`
	Routes  map[string]map[string]string `yaml:"routes"`
}

func readLocal(path string) (*localFile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lf localFile
	if err := yaml.Unmarshal(data, &lf); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &lf, nil
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// whose words an upstream's target the way the rest of the output does: the
// copy in the main checkout, or the one here. (The flags and the config say
// baseline and local.)
func whose(t plan.Target) string {
	if t == plan.Local {
		return "here"
	}
	return "main"
}

func printPlan(t *target, p *plan.Plan) {
	kind := "worktree"
	if p.Slot == 0 {
		kind = "main checkout"
	}
	fmt.Printf("%s, slot %d: %s\n", kind, p.Slot, t.checkout.Root)
	stacked := len(p.Layers) > 1
	if stacked {
		names := []string{t.stack.Hydra.BaseRef}
		for _, l := range p.Layers {
			names = append(names, fmt.Sprintf("%s (%d changed)", l.Name, len(l.Changed)))
		}
		fmt.Printf("stack: %s\n", strings.Join(names, " <- "))
	}
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERVICE\tRUNS\tPORT\tWHY")
	for _, s := range p.Services {
		where := "main"
		if s.Routed {
			where = "main, routed"
		}
		if s.Local {
			where = "here"
			if s.Disabled {
				where = "here (off)"
			}
		}
		port := "-"
		if s.Port != 0 {
			port = fmt.Sprint(s.Port)
		}
		if stacked && s.Local {
			where += " from " + s.Layer
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Name, where, port, s.Why)
	}
	w.Flush()

	if p.Slot > 0 {
		fmt.Println("\nwiring:")
		w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		n := 0
		for _, s := range p.Services {
			for _, u := range s.Upstreams {
				n++
				fmt.Fprintf(w, "  %s\t%s\t-> %s (%s)\t%s\t%s\n", s.Name, u.Env, u.Service, whose(u.Target), u.Value, u.Why)
			}
		}
		w.Flush()
		if n == 0 {
			local := false
			for _, s := range p.Services {
				local = local || s.Local
			}
			if local {
				fmt.Println("  (what runs here points at no other service)")
			} else {
				fmt.Println("  (nothing runs here)")
			}
		}
		for _, s := range p.Services {
			if src := t.stack.Services[s.Source]; s.Local && src != nil && src.Kind == config.KindSupabase {
				ports := t.stack.SlotPorts(src, p.Slot)
				fmt.Printf("  %s is this worktree's own Supabase stack (API :%d, database :%d), with its migrations applied after each start\n",
					s.Name, ports[""], ports["db"])
			}
			if src := t.stack.Services[s.Source]; s.Prepare != "" && src != nil && src.Kind == config.KindTemporal && src.Prepare == "" {
				fmt.Printf("  %s first creates Temporal namespace %s, with %s's search attributes\n", s.Name,
					plan.Value(t.stack, src, p.Slot, ""), plan.Value(t.stack, src, 0, ""))
				continue
			}
			if s.Prepare != "" {
				cmd := s.Prepare
				if r := []rune(cmd); len(r) > 90 {
					cmd = string(r[:89]) + "…"
				}
				fmt.Printf("  %s first runs: %s\n", s.Name, cmd)
			}
		}
	}
	for _, s := range p.Services {
		if s.Local && s.Disabled && p.Slot > 0 {
			fmt.Printf("\n%s is off until someone starts it (disabled: true): `hydra up %s` runs it here, or `hydra process start %s` once this stack is up.\n", s.Name, s.Name, s.Name)
		}
	}
	for _, warn := range p.Warnings {
		fmt.Println("\nwarning:", warn)
	}
	if len(p.Unclaimed) > 0 {
		fmt.Println("\nnote:", unclaimedNote(p.Unclaimed))
	}
	if pr := skewProblem(t.stack, p); pr != nil {
		fmt.Println()
		printProblems(os.Stdout, []problem{*pr})
	}
}
