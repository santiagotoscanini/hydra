package main

import (
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/gitx"
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
}

func parsePC(args []string) pcArgs {
	var a pcArgs
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
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
			out, local, err := renderFor(co, file, a.names)
			if err != nil {
				return err
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

// renderFor plans and renders the checkout and returns the rendered file.
func renderFor(co gitx.Checkout, file string, names []string) (string, map[string]bool, error) {
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
	}
	if p.Slot > 0 {
		fmt.Fprintf(os.Stderr, "hydra: slot %d, running here: %s\n", p.Slot, strings.Join(orNone(p.Local()), ", "))
		routedNote(t, p)
		for _, w := range p.Warnings {
			fmt.Fprintln(os.Stderr, "hydra: warning:", w)
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
	return syscall.Exec(pc, append([]string{"process-compose"}, args...), os.Environ())
}
