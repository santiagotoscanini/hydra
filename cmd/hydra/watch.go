package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/plan"
	"github.com/santiagotoscanini/hydra/internal/render"
	"github.com/santiagotoscanini/hydra/internal/slots"
)

// watchCommand is the command line of the hydra-watch process for t. It must
// come out the same on every render, or each update would restart it.
func watchCommand(t *target, o options) (string, error) {
	exe, err := stableExecutable()
	if err != nil {
		return "", err
	}
	args := []string{exe, "watch", "--root", t.checkout.Root, "-f", t.file}
	for _, w := range o.with {
		args = append(args, "--with", w)
	}
	for _, w := range o.without {
		args = append(args, "--without", w)
	}
	for _, r := range o.routes {
		args = append(args, "--route", r)
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shQuote(a)
	}
	return "exec " + strings.Join(quoted, " "), nil
}

// stableExecutable returns a path to this binary that survives upgrades:
// the `hydra` on PATH when it is this same binary (Homebrew's
// /opt/homebrew/bin/hydra, or a `make link` symlink), rather than the
// versioned Cellar directory it resolves to.
func stableExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if onPath, err := exec.LookPath("hydra"); err == nil {
		a, errA := os.Stat(onPath)
		b, errB := os.Stat(exe)
		if errA == nil && errB == nil && os.SameFile(a, b) {
			return filepath.Abs(onPath)
		}
	}
	return exe, nil
}

// watchCmd runs inside a worktree's process-compose project. Every interval
// it plans the stack again and, when the result differs from the running
// file, hands process-compose the new one with `project update`. That covers
// both transitions:
//
//	unchanged -> changed: the service is added here, and services already
//	                      here that point at it are restarted on the new URL
//	changed -> unchanged: the service is removed (stopped) and everything is
//	                      pointed back at the baseline
//
// Edits to a service that is already changed are left to its own reload
// (process-compose `watch`, next dev, ...).
func watchCmd(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	file := fs.String("f", "", "")
	root := fs.String("root", "", "")
	interval := fs.Duration("interval", 2*time.Second, "")
	once := fs.Bool("once", false, "")
	var with, without, routes multi
	fs.Var(&with, "with", "")
	fs.Var(&without, "without", "")
	fs.Var(&routes, "route", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	t, err := resolve(*root, *file)
	if err != nil {
		return err
	}
	if t.checkout.IsMain() {
		return fmt.Errorf("watch runs in a worktree; the main checkout runs everything already")
	}
	o := options{slot: -1, with: with, without: without, routes: routes}
	cmd, err := watchCommand(t, o)
	if err != nil {
		return err
	}
	out := filepath.Join(t.checkout.Root, ".process-compose", "hydra.yaml")
	sock := socketPath(t.checkout)

	logf("watching %s every %s", describeStack(t), *interval)
	parent := os.Getppid()
	for {
		if orphaned(parent) {
			logf("the process-compose that started this watcher is gone; stopping")
			return nil
		}
		if slots.Removed(t.checkout.Root) {
			// The worktree was removed with its stack still up. Nothing here
			// can work without its checkout, and the ports belong to the
			// slot's next worktree.
			stopGone(t)
			return nil
		}
		if err := step(t, o, cmd, out, sock); err != nil {
			logf("%v", err) // a failed git call (mid-rebase, say) shouldn't end the watch
		}
		if *once {
			return nil
		}
		// Look for the removal more often than for changes: every file this
		// stack writes while git deletes the worktree is one git can't delete
		// the directory for, and the watcher's own are among them.
		for waited := time.Duration(0); waited < *interval && !slots.Removed(t.checkout.Root); waited += goneCheck {
			time.Sleep(min(goneCheck, *interval-waited))
		}
	}
}

const goneCheck = 250 * time.Millisecond

// orphaned reports whether the process that started this one is gone.
// process-compose starts the watcher and the router; when it is killed they
// live on under another parent, and would fight the ones its successor
// starts: two watchers updating one project, two routers after one port.
func orphaned(parent int) bool { return os.Getppid() != parent }

func step(t *target, o options, cmd, out, sock string) error {
	// Re-read the stack file: an edit to x-hydra applies without a restart.
	fresh, err := resolve(t.checkout.Root, t.file)
	if err != nil {
		return err
	}
	// A worktree removed under its running stack can leave its folder behind
	// without the `.git` that made it one. git then answers for whatever
	// repository lies above the folder, and for a worktree kept inside the
	// main checkout that is the main checkout, whose plan is the whole stack.
	if fresh.checkout.Root != t.checkout.Root {
		return fmt.Errorf("%s is no longer a worktree: git finds %s there", t.checkout.Root, fresh.checkout.Root)
	}
	*t = *fresh
	p, err := t.build(o)
	if err != nil {
		return err
	}
	noteMain(t, p)
	data, err := t.render(p, cmd)
	if err != nil {
		return err
	}
	if slots.Removed(t.checkout.Root) {
		// It was removed while this looked: what git said on the way can't
		// be trusted, and nothing more is written into the folder.
		return fmt.Errorf("%s was removed", t.checkout.Root)
	}
	// Before the update: the router has to release the port of a routed
	// service that is about to start here.
	if err := publishRoutes(t, p); err != nil {
		return err
	}
	if err := syncEnvLocal(t, p); err != nil {
		return err
	}
	current, _ := os.ReadFile(out)
	if bytes.Equal(current, data) {
		migrateSupabase(t, p)
		return nil
	}
	if err := os.MkdirAll(filepath.Join(t.checkout.Root, render.WatchAnchor), 0o755); err != nil {
		return err
	}
	forgetPrepared(t, p, processesOf(current), processesOf(data))
	// In steps, so nothing starts before what it waits for (see applyProject).
	// A step that fails leaves the file at the last one that went through,
	// and the next tick carries on from there.
	err = applyProject(out, current, data, func(file string) error {
		update := exec.Command("process-compose", "project", "update", "-v", "-U", "-u", sock, "-f", file)
		update.Stdout, update.Stderr = os.Stdout, os.Stderr
		return update.Run()
	})
	if err != nil {
		return fmt.Errorf("project update: %w", err)
	}

	// Report against what was running (the old file), not a remembered plan.
	prev, now := running(current), running(data)
	for _, name := range sortedNames(now) {
		row := p.Get(name)
		switch {
		case prev[name] == "":
			logf("+ %s now runs here (%s)", name, row.Why)
		case prev[name] != now[name]:
			logf("~ %s now runs from %s", name, now[name])
		}
	}
	for _, name := range sortedNames(prev) {
		if now[name] == "" {
			logf("- %s stopped; main's copy is used again", name)
		}
	}
	migrateSupabase(t, p)
	return nil
}

// forgetPrepared removes the mark of a worktree's own database being ready
// (supabaseMarker) when an update is about to start that database: was and
// will are the processes before and after it. The database's own start
// removes the mark too, but only once it runs, and the step that waits for
// the mark may start first.
func forgetPrepared(t *target, p *plan.Plan, was, will map[string]any) {
	changed := changedProcesses(was, will)
	for _, row := range p.Services {
		src := t.stack.Services[row.Source]
		if !row.Local || src == nil || src.Kind != config.KindSupabase || (was != nil && !changed[row.Name]) {
			continue
		}
		root := t.checkout.Root
		if row.Root != "" {
			root = row.Root
		}
		os.Remove(supabaseMarker(root))
	}
}

// said is the last thing the watcher logged about each subject, so it says a
// thing once, when it changes.
var said = map[string]string{}

// noteMain logs, in the worktree's own log, when the main checkout's stack
// stops answering and when the branch falls out of step with main: both
// break what runs here without anything here changing.
func noteMain(t *target, p *plan.Plan) {
	say := func(key, bad, good string) {
		if said[key] == bad {
			return
		}
		was := said[key]
		said[key] = bad
		switch {
		case bad != "":
			logf("%s", bad)
		case was != "":
			logf("%s", good)
		}
	}
	down := ""
	if !mainClient(t.checkout.CommonDir).Alive() {
		down = "! main's stack isn't answering: what this worktree takes from it is down (cd " + shQuote(t.checkout.Main) + " && hydra up -D)"
	}
	say("main", down, "main's stack answers again")
	skew := ""
	if pr := skewProblem(t.stack, p); pr != nil {
		skew = "! " + strings.ReplaceAll(pr.Text, "\n", "\n    ") + "\n    fix: " + pr.Fix
	}
	say("skew", skew, "in step with main again")
}

// migrationsSeen is what the worktree's supabase/migrations looked like when
// the watcher last applied them, per service.
var migrationsSeen = map[string]string{}

// migrateSupabase applies new migrations to a worktree's own Supabase stack
// that is already running, when files in supabase/migrations appear or
// change: a stack applies migrations only when it starts.
//
// While the stack is starting (no mark of it being ready yet) there is
// nothing to do: its start applies every migration there is, and the
// database doesn't answer before that. The first look after it is ready
// applies them once more, in case a file landed while the start was ending;
// that says nothing unless it fails.
func migrateSupabase(t *target, p *plan.Plan) {
	for _, row := range p.Services {
		src := t.stack.Services[row.Source]
		if !row.Local || src == nil || src.Kind != config.KindSupabase {
			continue
		}
		root := t.checkout.Root
		if row.Root != "" {
			root = row.Root
		}
		if _, err := os.Stat(supabaseMarker(root)); err != nil {
			delete(migrationsSeen, row.Name)
			continue
		}
		now := dirFingerprint(filepath.Join(root, "supabase", "migrations"))
		prev, seen := migrationsSeen[row.Name]
		migrationsSeen[row.Name] = now
		if seen && prev == now {
			continue
		}
		cmd := exec.Command(t.stack.Hydra.Exe, "prepare", "supabase", "--migrate")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), t.stack.SupabaseEnv(src, p.Slot)...)
		cmd.Env = append(cmd.Env, "TMPDIR="+filepath.Join(root, "supabase", ".temp", "tmp"))
		var said bytes.Buffer
		cmd.Stdout, cmd.Stderr = &said, &said
		if seen {
			logf("supabase/migrations changed; applying them to this worktree's stack")
			cmd.Stdout, cmd.Stderr = io.MultiWriter(os.Stdout, &said), io.MultiWriter(os.Stderr, &said)
		}
		failed := migrationsFailed(root)
		if err := cmd.Run(); err != nil {
			if !seen {
				os.Stdout.Write(said.Bytes())
			}
			logf("applying migrations: %v (retried on the next change)", err)
			// For `hydra status`: nothing else would say that the database
			// here lacks what the files describe.
			writeFile(failed, lastLines(said.Bytes(), 8))
			continue
		}
		os.Remove(failed)
	}
}

// migrationsFailed is where the watcher leaves the end of what the Supabase
// CLI said when migrations didn't apply to a worktree's running database.
// It is gone once they apply, and when the database starts again.
func migrationsFailed(root string) string {
	return filepath.Join(root, ".process-compose", "supabase-migrations-failed")
}

// lastLines keeps the last n lines of out that say something about what
// went wrong.
func lastLines(out []byte, n int) []byte {
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		chatter := false
		for _, p := range []string{"Try rerunning the command", "A new version of Supabase CLI", "We recommend updating", "hydra: "} {
			chatter = chatter || strings.HasPrefix(l, p)
		}
		if strings.TrimSpace(l) != "" && !chatter {
			lines = append(lines, strings.TrimRight(l, " \t\r"))
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// dirFingerprint names every file in dir with its size and modification
// time.
func dirFingerprint(dir string) string {
	entries, _ := os.ReadDir(dir)
	var b strings.Builder
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			fmt.Fprintf(&b, "%s %d %d\n", e.Name(), info.Size(), info.ModTime().UnixNano())
		}
	}
	return b.String()
}

// running maps each real service in a rendered file to its working
// directory, leaving out stand-ins and hydra's own processes.
func running(data []byte) map[string]string {
	var doc struct {
		Processes map[string]struct {
			Namespace  string `yaml:"namespace"`
			WorkingDir string `yaml:"working_dir"`
		} `yaml:"processes"`
	}
	m := map[string]string{}
	if yaml.Unmarshal(data, &doc) != nil {
		return m
	}
	for name, p := range doc.Processes {
		if p.Namespace != "main" && p.Namespace != "baseline" && p.Namespace != "hydra" { // "baseline": files rendered by an older hydra
			m[name] = p.WorkingDir
		}
	}
	return m
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func describeStack(t *target) string {
	layers, _, _, err := t.layers()
	if err != nil || len(layers) < 2 {
		return t.checkout.Root
	}
	var names []string
	for _, l := range layers {
		names = append(names, l.Name)
	}
	return strings.Join(names, " <- ")
}

func logf(format string, args ...any) {
	fmt.Printf("%s hydra: %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

func shQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
