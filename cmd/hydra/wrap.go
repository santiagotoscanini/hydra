package main

import (
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/pc"
	"github.com/santiagotoscanini/hydra/internal/plan"
)

// process-compose subcommands that load the config file, so they get the
// rendered one. Others (down, attach, process ...) only talk to the server.
var loadsConfig = map[string]bool{"": true, "up": true, "run": true, "list": true, "graph": true, "info": true}

var subcommands = map[string]bool{
	"analyze": true, "attach": true, "completion": true, "down": true, "graph": true,
	"help": true, "info": true, "list": true, "namespace": true, "process": true,
	"project": true, "recipe": true, "run": true, "up": true, "version": true,
}

// Global process-compose flags that take a value as the next argument.
var valueFlags = map[string]bool{
	"-f": true, "--config": true, "-p": true, "--port": true, "-u": true, "--unix-socket": true,
	"-L": true, "--log-file": true, "-n": true, "--namespace": true, "-r": true, "--ref-rate": true,
	"-e": true, "--env": true, "--theme": true, "--token-file": true, "--shortcuts": true,
	"-S": true, "--sort": true, "--slow-ref-rate": true, "--address": true,
}

type pcArgs struct {
	sub   string   // subcommand, "" when implicit up
	file  string   // the user's -f, if any
	names []string // positional process names after up/run
	rest  []string // everything else, in order, minus -f
	force bool     // hydra's own --force: start despite what the checks found
}

func parsePC(args []string) pcArgs {
	var a pcArgs
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--force":
			a.force = true
			continue
		case arg == "-f" || arg == "--config":
			if i+1 < len(args) {
				a.file = args[i+1]
				i++
			}
			continue
		case strings.HasPrefix(arg, "-f=") || strings.HasPrefix(arg, "--config="):
			a.file = arg[strings.Index(arg, "=")+1:]
			continue
		case valueFlags[arg]:
			a.rest = append(a.rest, arg)
			if i+1 < len(args) {
				a.rest = append(a.rest, args[i+1])
				i++
			}
			continue
		case !strings.HasPrefix(arg, "-") && a.sub == "" && subcommands[arg] && len(a.names) == 0:
			a.sub = arg
			continue
		case !strings.HasPrefix(arg, "-") && (a.sub == "up" || a.sub == "run"):
			a.names = append(a.names, arg)
		}
		a.rest = append(a.rest, arg)
	}
	return a
}

// wrap runs process-compose for this checkout.
func wrap(args []string) error {
	pc, err := exec.LookPath("process-compose")
	if err != nil {
		return fmt.Errorf("process-compose is not installed")
	}
	a := parsePC(args)

	co, err := gitx.Find(".")
	if err != nil { // not a git checkout: nothing to be smart about
		return execPC(pc, args)
	}

	// A worktree's process-compose server always listens on that worktree's
	// own socket, whatever the config says, so no command run from a
	// worktree (down above all) can reach the baseline's server on :8099.
	var conn []string
	if !co.IsMain() {
		conn = []string{"-U", "-u", socketPath(co)}
	}

	var final []string
	if a.sub != "" {
		final = append(final, a.sub)
	}

	if loadsConfig[a.sub] {
		file := a.file
		if file == "" {
			file = defaultFile(co)
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if !config.HasHydra(data) {
			if !co.IsMain() {
				fmt.Fprintf(os.Stderr, "hydra: %s has no x-hydra block; running everything, which will clash with the baseline's ports\n", file)
			}
			final = append(final, "-f", file)
		} else {
			starting := a.sub == "" || a.sub == "up" || a.sub == "run"
			out, local, err := renderFor(co, file, a.names, starting, a.force)
			if err != nil {
				return err
			}
			if starting && co.IsMain() {
				recordStart(co, a.rest)
			}
			final = append(final, "-f", out)
			a.rest = keepRunnable(a.rest, a.names, local)
			if len(a.names) > 0 && local["hydra-watch"] {
				// `up web` starts only web and what it needs; keep the watcher.
				a.rest = append(a.rest, "hydra-watch")
			}
		}
	}
	final = append(final, conn...)
	final = append(final, a.rest...)

	if err := os.Chdir(co.Root); err != nil { // relative log and watch paths
		return err
	}
	return execPC(pc, final)
}

// recordStart notes, when the main checkout's stack is about to start, the
// commit it starts on and where its process-compose will listen: worktrees
// check both. A stack that is already up keeps its record.
func recordStart(co gitx.Checkout, args []string) {
	host, port := pcAddress(args)
	if pc.TCP(host, port).Alive() {
		return
	}
	head, err := gitx.Head(co.Root)
	if err != nil {
		return
	}
	if err := writeBaseline(co.CommonDir, baselineRecord{Commit: head, At: time.Now(), Address: host, Port: port}); err != nil {
		fmt.Fprintln(os.Stderr, "hydra: warning: can't record the baseline's commit:", err)
	}
}

// preflight refuses to start a worktree on a main stack that is down,
// broken or out of step with the branch: everything `hydra status` would
// flag before anything runs here.
func preflight(co gitx.Checkout, t *target, p *plan.Plan, force bool) error {
	ms, err := loadMain(co)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hydra: warning: can't check the main checkout:", err)
		return nil
	}
	problems := worktreeProblems(ms, &wtState{target: t, Plan: p}, false)
	if len(problems) == 0 {
		return nil
	}
	printProblems(os.Stderr, problems)
	if len(blocking(problems)) > 0 && !force {
		return fmt.Errorf("not starting this worktree: fix what is marked ✗ above, or start anyway with --force")
	}
	return nil
}

// renderFor plans and renders the checkout and returns the rendered file.
// With starting, a worktree is checked against the main checkout first.
func renderFor(co gitx.Checkout, file string, names []string, starting, force bool) (string, map[string]bool, error) {
	t, err := resolve(co.Root, file)
	if err != nil {
		return "", nil, err
	}
	o := options{slot: -1}
	watch := ""
	if !co.IsMain() {
		o.with = names // `hydra up web` means "run web here too"
		if watch, err = watchCommand(t, o); err != nil {
			return "", nil, err
		}
	}
	p, err := t.build(o)
	if err != nil {
		return "", nil, err
	}
	if starting && !co.IsMain() {
		if err := preflight(co, t, p, force); err != nil {
			return "", nil, err
		}
	}
	data, err := t.render(p, watch)
	if err != nil {
		return "", nil, err
	}
	out := filepath.Join(co.Root, ".process-compose", "hydra.yaml")
	if err := writeFile(out, data); err != nil {
		return "", nil, err
	}
	if !co.IsMain() {
		if err := publishRoutes(t, p); err != nil {
			return "", nil, err
		}
		if err := syncEnvLocal(t, p); err != nil {
			return "", nil, err
		}
	}
	if p.Slot > 0 {
		fmt.Fprintf(os.Stderr, "hydra: slot %d, running here: %s\n", p.Slot, strings.Join(orNone(p.Local()), ", "))
		routedNote(t, p)
		if !starting { // the checks before a start already said these
			for _, w := range p.Warnings {
				fmt.Fprintln(os.Stderr, "hydra: warning:", w)
			}
		}
	}
	local := map[string]bool{}
	for _, n := range p.Local() {
		local[n] = true
	}
	if watch != "" {
		local["hydra-watch"] = true
	}
	return out, local, nil
}

// keepRunnable drops process names given to `up`/`run` that don't run in
// this checkout (shared, or excluded), which process-compose would reject.
func keepRunnable(rest, names []string, local map[string]bool) []string {
	drop := map[string]bool{}
	for _, n := range names {
		if !local[n] {
			drop[n] = true
			fmt.Fprintf(os.Stderr, "hydra: %s doesn't run in this checkout (shared or excluded); the baseline's copy is used\n", n)
		}
	}
	var out []string
	for _, r := range rest {
		if !drop[r] {
			out = append(out, r)
		}
	}
	return out
}

func socketPath(co gitx.Checkout) string {
	h := fnv.New32a()
	h.Write([]byte(co.Root))
	return filepath.Join(os.TempDir(), fmt.Sprintf("hydra-%08x.sock", h.Sum32()))
}

func orNone(xs []string) []string {
	if len(xs) == 0 {
		return []string{"(nothing, everything comes from the baseline)"}
	}
	return xs
}

func execPC(pc string, args []string) error {
	if os.Getenv("HYDRA_DRY_RUN") != "" {
		fmt.Println(strings.Join(append([]string{"process-compose"}, args...), " "))
		return nil
	}
	return syscall.Exec(pc, append([]string{"process-compose"}, args...), withoutAgentMarks(os.Environ()))
}

// agentMarks are the variables tools read to tell that an AI agent's shell
// started them. Next.js, for one, prints a notice for agents and exits on
// its first start. A stack has to run the same whoever typed `hydra up`, so
// its processes don't inherit them. A trailing * matches a prefix.
var agentMarks = []string{
	"AI_AGENT", "CLAUDECODE", "CLAUDE_CODE", "CLAUDE_CODE_*", "CLAUDE_AGENT_SDK_VERSION",
	"CURSOR_AGENT", "CURSOR_TRACE_ID", "CODEX_SANDBOX", "CODEX_SANDBOX_*", "CODEX_THREAD_ID",
	"GEMINI_CLI", "AUGMENT_AGENT", "OPENCODE_CLIENT", "REPL_ID",
}

func withoutAgentMarks(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		marked := false
		for _, m := range agentMarks {
			if prefix, ok := strings.CutSuffix(m, "*"); ok {
				marked = marked || strings.HasPrefix(name, prefix)
			} else {
				marked = marked || name == m
			}
		}
		if !marked {
			out = append(out, kv)
		}
	}
	return out
}
