package main

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"net"
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
	"github.com/santiagotoscanini/hydra/internal/render"
	"github.com/santiagotoscanini/hydra/internal/stack"
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
	sub    string   // subcommand, "" when implicit up
	file   string   // the user's -f, if any
	names  []string // positional process names after up/run
	nameAt []int    // where each of them sits in rest
	rest   []string // everything else, in order, minus -f
	force  bool     // hydra's own --force: start despite what the checks found
}

// dropNames removes from rest the process names that keep rejects. It goes
// by position, so a flag's value that happens to equal a name stays.
func (a *pcArgs) dropNames(keep func(name string) bool) {
	drop := map[int]bool{}
	for i, at := range a.nameAt {
		if !keep(a.names[i]) {
			drop[at] = true
		}
	}
	var rest []string
	var nameAt []int
	for i, r := range a.rest {
		if drop[i] {
			continue
		}
		for _, at := range a.nameAt {
			if at == i {
				nameAt = append(nameAt, len(rest))
			}
		}
		rest = append(rest, r)
	}
	a.rest, a.nameAt = rest, nameAt
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
			a.nameAt = append(a.nameAt, len(a.rest))
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
	} else {
		conn = mainConn(co, a.rest)
	}
	if a.sub == "down" && contains(a.rest, "--worktrees") {
		return downWorktrees(co)
	}
	if a.sub == "down" && !ownServer(co, a.rest).Alive() {
		return downStopped(co, a.file)
	}
	if a.sub == "process" && len(a.rest) > 1 && (a.rest[0] == "restart" || a.rest[0] == "start") {
		// An earlier copy of the service that still holds its port would
		// fail this start.
		var names []string
		for _, arg := range a.rest[1:] {
			if !strings.HasPrefix(arg, "-") {
				names = append(names, arg)
			}
		}
		clearLeftoversNamed(co, a.file, names, os.Stderr)
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
				fmt.Fprintf(os.Stderr, "hydra: %s has no x-hydra block; running everything, which will clash with main's ports\n", file)
			}
			final = append(final, "-f", file)
		} else {
			starting := a.sub == "" || a.sub == "up" || a.sub == "run"
			if starting {
				// Before a slot is taken: a removed worktree may still hold one,
				// with its stack running on that slot's ports.
				reapGone(co, os.Stderr)
			}
			if a.sub == "" || a.sub == "up" {
				if c := ownServer(co, a.rest); c.Alive() {
					return alreadyUp(co, file, a, c, pc, conn)
				}
			}
			out, local, err := renderFor(co, file, a.names, starting, a.force)
			if err != nil {
				return err
			}
			if starting && co.IsMain() {
				recordStart(co, a.rest)
				ensureFrontCert(co, file)
			}
			final = append(final, "-f", out)
			// Names that don't run in this checkout (shared, or excluded) are
			// dropped: process-compose would reject them.
			a.dropNames(func(n string) bool {
				if !local[n] {
					fmt.Fprintf(os.Stderr, "hydra: %s doesn't run in this checkout (shared or excluded); main's copy is used\n", n)
				}
				return local[n]
			})
			switch {
			case !co.IsMain() && a.sub != "run":
				// In a worktree, `hydra up web` means "run web here as well":
				// the name is in the rendered file now, and the whole project
				// starts. Handing the name on would have process-compose start
				// web alone and leave what the branch changed switched off.
				a.dropNames(func(string) bool { return false })
			case len(a.nameAt) > 0 && local["hydra-watch"]:
				// `run web` starts only web and what it needs; keep the watcher.
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
		fmt.Fprintln(os.Stderr, "hydra: warning: can't record the commit main's stack starts on:", err)
	}
}

// ensureFrontCert makes hydra's CA and certificate before the main stack
// starts, if it has TLS fronts: processes are handed the CA's path, and the
// file had better be there.
func ensureFrontCert(co gitx.Checkout, file string) {
	t, err := resolve(co.Root, file)
	if err != nil || !t.stack.HasTLS() {
		return
	}
	files, err := tlsFilesFor(t)
	if err == nil {
		err = ensureTLS(files)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hydra: warning: no certificate for the TLS fronts:", err)
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
	o := options{slot: -1, take: starting}
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
	// What hydra-watch's own watch points at (see render.WatchAnchor).
	if err := os.MkdirAll(filepath.Join(co.Root, render.WatchAnchor), 0o755); err != nil {
		return "", nil, err
	}
	if err := writeFile(out, data); err != nil {
		return "", nil, err
	}
	if co.IsMain() {
		if err := writeFile(startedFromPath(co.Root), t.stack.Source); err != nil {
			return "", nil, err
		}
	}
	if !co.IsMain() {
		if err := publishRoutes(t, p); err != nil {
			return "", nil, err
		}
		if err := syncEnvLocal(t, p); err != nil {
			return "", nil, err
		}
		if starting {
			// The stack is about to start, and its own database with it: what
			// an earlier run left behind says nothing about this one.
			forgetPrepared(t, p, nil, nil)
		}
	}
	if starting {
		// A process-compose that was killed left its services running, and
		// what still holds a port would fail its service's start.
		clearLeftoversOf(t, p, os.Stderr)
	}
	if p.Slot > 0 {
		var on, off []string
		for _, row := range p.Services {
			switch {
			case row.Local && row.Disabled:
				off = append(off, row.Name)
			case row.Local:
				on = append(on, row.Name)
			}
		}
		line := fmt.Sprintf("hydra: slot %d, running here: %s", p.Slot, strings.Join(orNone(on), ", "))
		if len(off) > 0 {
			line += fmt.Sprintf("; off until started (`hydra up NAME`): %s", strings.Join(off, ", "))
		}
		fmt.Fprintln(os.Stderr, line)
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

func socketPath(co gitx.Checkout) string { return socketFor(co.Root) }

// socketFor is where the process-compose server of the worktree at root
// listens. It is worked out from the path alone, so it can be found again
// after the worktree is removed.
func socketFor(root string) string {
	h := fnv.New32a()
	h.Write([]byte(root))
	return filepath.Join(os.TempDir(), fmt.Sprintf("hydra-%08x.sock", h.Sum32()))
}

// ownServer is this checkout's process-compose server: a worktree's on its
// socket; the main checkout's where these arguments and this environment
// would start it, or, if nothing answers there, where hydra recorded that it
// was started (another shell may have had another PC_PORT_NUM).
func ownServer(co gitx.Checkout, args []string) *pc.Client {
	if !co.IsMain() {
		return pc.Unix(socketPath(co))
	}
	here := pc.TCP(pcAddress(args))
	if here.Alive() {
		return here
	}
	if rec := mainClient(co.CommonDir); rec.Where != here.Where && rec.Alive() {
		if files, err := rec.Files(); err == nil && len(files) > 0 && sameFile(files[0], renderedPath(co.Root)) {
			return rec
		}
	}
	return here
}

// mainConn is what a command in the main checkout needs to reach its stack
// when that runs somewhere else than this shell would look: the flags that
// name the recorded address. Nothing when the shell already points there, or
// when the command line says where to go.
func mainConn(co gitx.Checkout, args []string) []string {
	if len(connFlags(args)) > 0 {
		return nil
	}
	c := ownServer(co, args)
	if c.Where == pc.TCP(pcAddress(args)).Where {
		return nil
	}
	host, port, err := net.SplitHostPort(c.Where)
	if err != nil {
		return nil
	}
	return []string{"--address", host, "-p", port}
}

// alreadyUp is `hydra up` in a checkout whose stack is running. A second
// process-compose would fail on the taken socket or port, in the background
// and unseen with -D, so hydra answers instead:
//
//   - `hydra up NAME` runs NAME here as well, as it would have at the start;
//   - `hydra up` in a terminal opens the running stack;
//   - `hydra up -D` says it is up, and exits 0: starting twice is not an error.
func alreadyUp(co gitx.Checkout, file string, a pcArgs, c *pc.Client, pcBin string, conn []string) error {
	// Something answers there. Is it this checkout's stack?
	if files, err := c.Files(); err == nil && len(files) > 0 && !sameFile(files[0], renderedPath(co.Root)) {
		return fmt.Errorf("another process-compose project is running on %s (from %s); give this one its own port: hydra up -p PORT, or PC_PORT_NUM", c.Where, files[0])
	}
	if len(a.names) > 0 {
		return alsoRun(co, file, a.names, c)
	}
	if !detached(a.rest) && isTerminal(os.Stdout) && isTerminal(os.Stdin) {
		fmt.Fprintln(os.Stderr, "hydra: this checkout's stack is already up; opening it.")
		return execPC(pcBin, append(append([]string{"attach"}, conn...), connFlags(a.rest)...))
	}
	fmt.Fprintln(os.Stderr, "hydra: this checkout's stack is already up. `hydra status` says how it is doing; `hydra attach` opens it.")
	return nil
}

// alsoRun is `hydra up NAME...` while the stack is up. In a worktree the
// names join the ones the stack was started with, the file is rendered again
// and process-compose takes the difference; the watcher restarts with the
// longer list, so it stays that way. In the main checkout every process is
// already in the project, and one that is off is started.
func alsoRun(co gitx.Checkout, file string, names []string, c *pc.Client) error {
	if !co.IsMain() {
		t, err := resolve(co.Root, file)
		if err != nil {
			return err
		}
		o := runningOptions(co.Root)
		for _, n := range names {
			if !contains(o.with, n) {
				o.with = append(o.with, n)
			}
		}
		watch, err := watchCommand(t, o)
		if err != nil {
			return err
		}
		if _, err := step(t, o, watch, renderedPath(co.Root), socketPath(co)); err != nil {
			return err
		}
	}
	procs, err := c.Processes()
	if err != nil {
		return err
	}
	state := map[string]pc.Process{}
	for _, p := range procs {
		state[p.Name] = p
	}
	for _, n := range names {
		p, ok := state[n]
		switch {
		case !ok && co.IsMain():
			fmt.Fprintf(os.Stderr, "hydra: there is no process named %s in this stack.\n", n)
		case !ok:
			fmt.Fprintf(os.Stderr, "hydra: %s doesn't run in this checkout (shared or excluded); main's copy is used\n", n)
		case p.Health() == pc.Off:
			if err := c.Start(n); err != nil {
				return fmt.Errorf("starting %s: %w", n, err)
			}
			fmt.Fprintf(os.Stderr, "hydra: started %s here.\n", n)
		default:
			fmt.Fprintf(os.Stderr, "hydra: %s runs here (%s).\n", n, p.Describe())
		}
	}
	return nil
}

// downWorktrees is `hydra down --worktrees`: it stops the stack of every
// worktree of this repository and leaves the main checkout's running. A
// worktree's stack costs memory for as long as it is up, whether or not
// anyone still works there; this is the end of the day in one command.
// Nothing is lost: `hydra up -D` in a worktree brings its stack back.
func downWorktrees(co gitx.Checkout) error {
	reapGone(co, os.Stdout)
	wts, err := stack.List(co.Main)
	if err != nil {
		return err
	}
	// Which worktrees still have a database up is asked once, and only when
	// a stack that isn't running comes along: the Supabase CLI takes its time.
	var up map[string]bool
	database := func(root string) bool {
		if up == nil {
			up = map[string]bool{}
			if st := mainStackOf(co); st != nil && hasKind(st, config.KindSupabase) {
				up = supabaseUp()
			}
		}
		real, _ := filepath.EvalSymlinks(root)
		return up[root] || up[real]
	}
	stopped, failed := 0, 0
	for _, wt := range wts {
		wco, err := gitx.Find(wt.Root)
		if err != nil || wco.IsMain() {
			continue
		}
		sock := socketPath(wco)
		if !pc.Unix(sock).Alive() {
			// Not running, or killed: then part of it still is.
			var said bytes.Buffer
			if slotOf(wco) >= 0 && stopRemains(wco, "", database(wco.Root), &said) {
				fmt.Printf("%s: its process-compose was gone, part of its stack wasn't\n%s", filepath.Base(wco.Root), indent(said.String(), "  "))
				stopped++
			}
			continue
		}
		fmt.Printf("stopping %s ... ", filepath.Base(wco.Root))
		if err := downSocket(sock, 3*time.Minute); err != nil {
			fmt.Println("FAILED:", err)
			failed++
			continue
		}
		fmt.Println("stopped")
		stopped++
	}
	switch {
	case failed > 0:
		return fmt.Errorf("%d worktree stack%s didn't stop", failed, plural(failed))
	case stopped == 0:
		fmt.Println("no worktree's stack is running.")
	default:
		fmt.Printf("stopped %d worktree stack%s; the main checkout's stack is untouched.\n", stopped, plural(stopped))
	}
	return nil
}

// downStopped is `hydra down` where process-compose isn't running. After a
// clean stop that is all there is to say. After a kill, part of the stack
// is still there.
func downStopped(co gitx.Checkout, file string) error {
	if !stopRemains(co, file, true, os.Stderr) {
		fmt.Fprintln(os.Stderr, "hydra: this checkout's stack isn't running; nothing to stop.")
	}
	return nil
}

// indent puts prefix before every line of text.
func indent(text, prefix string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(text, "\n") {
		if line != "" {
			b.WriteString(prefix + line)
		}
	}
	return b.String()
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// detached reports whether process-compose was asked not to take the
// terminal: -D, or the TUI switched off.
func detached(args []string) bool {
	for _, a := range args {
		switch a {
		case "-D", "--detached", "--detached-with-tui", "-t=false", "--tui=false":
			return true
		}
	}
	return false
}

// connFlags keeps the flags that say where a process-compose server is.
func connFlags(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		name, _, inline := strings.Cut(args[i], "=")
		switch name {
		case "-p", "--port", "--address", "-u", "--unix-socket", "--token-file":
			out = append(out, args[i])
			if !inline && i+1 < len(args) {
				out = append(out, args[i+1])
				i++
			}
		case "-U", "--use-uds":
			out = append(out, args[i])
		}
	}
	return out
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

func orNone(xs []string) []string {
	if len(xs) == 0 {
		return []string{"(nothing, everything comes from main)"}
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
