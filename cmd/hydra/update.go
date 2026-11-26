package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/pc"
	"github.com/santiagotoscanini/hydra/internal/plan"
	"github.com/santiagotoscanini/hydra/internal/stack"
	"gopkg.in/yaml.v3"
)

// updateCmd is the one deliberate way the main checkout moves:
//
//  1. pull main (fast-forward only),
//  2. bring main's running stack to the new commit: reload its
//     configuration, rerun the jobs and restart the services whose files the
//     new commits touched, in dependency order,
//  3. merge main into every worktree that is behind it, so each uses main's
//     services at the version its branch is built on.
//
// It never resolves a conflict, never touches a worktree with uncommitted
// work, and never pushes. With --here, run in a worktree, it does only the
// third step, for that worktree.
func updateCmd(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	var dry, noPull, mainOnly, here bool
	fs.BoolVar(&dry, "dry-run", false, "")
	fs.BoolVar(&dry, "n", false, "")
	fs.BoolVar(&noPull, "no-pull", false, "")
	fs.BoolVar(&mainOnly, "main-only", false, "")
	fs.BoolVar(&here, "here", false, "")
	timeout := fs.Duration("timeout", 15*time.Minute, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	co, err := gitx.Find(".")
	if err != nil {
		return err
	}
	if here && co.IsMain() {
		return fmt.Errorf("--here is for a worktree; in the main checkout, `hydra update` pulls main and brings everything along")
	}
	if here && mainOnly {
		return fmt.Errorf("--here and --main-only leave nothing to do together")
	}
	if !dry {
		unlock, err := lockUpdate(co.CommonDir)
		if err != nil {
			return err
		}
		defer unlock()
		reapGone(co, os.Stdout) // worktrees removed since the last start
	}
	ms, err := loadMain(co)
	if err != nil {
		return err
	}
	main, base := ms.target.checkout.Root, ms.target.stack.Hydra.BaseRef
	would := ""
	if dry {
		would = "would be "
		fmt.Println("(dry run: nothing is pulled, restarted or merged)")
	}
	if here {
		// Main stays where it is; only this worktree catches up with it.
		needHand, err := updateWorktrees(co, ms, ms.Head, dry, *timeout, co.Root)
		if err != nil {
			return err
		}
		fmt.Println()
		switch {
		case needHand > 0:
			fmt.Println("this worktree needs a hand (above).")
			return errProblems{}
		case dry:
			fmt.Println("Run `hydra update --here` to do it.")
		default:
			fmt.Println("ok: this worktree is up to date with main's checkout (main itself wasn't pulled).")
		}
		return nil
	}

	// 1. The main checkout.
	if ms.Branch != base {
		on := "a detached commit"
		if ms.Branch != "" {
			on = "branch " + ms.Branch
		}
		return fmt.Errorf("the main checkout (%s) is on %s; check out %s there first", main, on, base)
	}
	ms.Dirty = withoutCLIEdit(ms, dry)
	if len(ms.Dirty) > 0 {
		return fmt.Errorf("the main checkout (%s) has uncommitted changes (%s); commit, stash or discard them first", main, someFiles(ms.Dirty))
	}
	target := ms.Head // the commit main ends up on
	switch upstream, _ := gitOut(main, "rev-parse", "--verify", "--quiet", "--abbrev-ref", base+"@{upstream}"); {
	case noPull:
		fmt.Printf("main  %s  at %s (not pulling)\n", main, gitx.Short(ms.Head))
	case upstream == "":
		fmt.Printf("main  %s  at %s (%s has no upstream, nothing to pull)\n", main, gitx.Short(ms.Head), base)
	case dry:
		if _, err := gitOut(main, "fetch", "--quiet"); err != nil {
			return err
		}
		if target, err = gitOut(main, "rev-parse", upstream); err != nil {
			return err
		}
		if !gitx.IsAncestor(main, ms.Head, target) {
			return fmt.Errorf("%s has commits %s doesn't have; a fast-forward pull would fail", base, upstream)
		}
		fmt.Printf("main  %s  %s\n", main, moved(main, ms.Head, target, "would move"))
	default:
		pull := exec.Command("git", "-C", main, "pull", "--ff-only", "--quiet")
		var stderr bytes.Buffer
		pull.Stderr = &stderr
		if err := pull.Run(); err != nil {
			return fmt.Errorf("git pull --ff-only in %s: %s", main, strings.TrimSpace(stderr.String()))
		}
		if target, err = gitx.Head(main); err != nil {
			return err
		}
		fmt.Printf("main  %s  %s\n", main, moved(main, ms.Head, target, "moved"))
	}

	// 2. Main's stack. Reload after the pull: the stack file may be tracked.
	failed := false
	if !dry {
		if ms, err = loadMain(co); err != nil {
			return err
		}
	}
	switch {
	case !ms.Up:
		fmt.Printf("\nmain's stack isn't running; `cd %s && hydra up -D` starts it on %s.\n", shQuote(main), gitx.Short(target))
	default:
		fmt.Println()
		ok, err := updateMainStack(ms, target, dry, *timeout)
		if err != nil {
			return err
		}
		failed = !ok
	}

	// 3. The worktrees.
	needHand := 0
	if !mainOnly && !failed {
		if needHand, err = updateWorktrees(co, ms, target, dry, *timeout, ""); err != nil {
			return err
		}
	}

	fmt.Println()
	switch {
	case failed:
		fmt.Println("main's stack is NOT up to date: fix the failure above and run `hydra update` again. The worktrees were left alone.")
		return errProblems{}
	case dry:
		fmt.Println("Run `hydra update` to do it.")
	case needHand > 0:
		fmt.Printf("main is up to date. %d worktree%s need%s a hand (above); `hydra status --all` shows where each stands.\n",
			needHand, plural(needHand), map[bool]string{true: "s", false: ""}[needHand == 1])
	case mainOnly:
		fmt.Println("ok: main is " + would + "up to date. The worktrees were left alone (--main-only).")
	default:
		fmt.Println("ok: main and every worktree are " + would + "up to date.")
	}
	return nil
}

// moved describes how a checkout went from one commit to another.
func moved(dir, from, to, verb string) string {
	if from == to {
		return "already up to date at " + gitx.Short(to)
	}
	n, _ := gitOut(dir, "rev-list", "--count", from+".."+to)
	return fmt.Sprintf("%s %s → %s (%s)", verb, gitx.Short(from), gitx.Short(to), commits(n))
}

// commits is "1 commit" or "4 commits".
func commits(n string) string {
	if n == "1" {
		return "1 commit"
	}
	return n + " commits"
}

// lockUpdate makes sure only one update runs per repository.
func lockUpdate(commonDir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(commonDir, "hydra-update.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another `hydra update` is running for this repository")
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// updateMainStack brings main's running stack to commit to: the current
// configuration, then every process whose files changed since the commit
// the stack was last brought to. It reports whether the stack ended up
// healthy, and only then records the new commit.
func updateMainStack(ms *mainState, to string, dry bool, timeout time.Duration) (bool, error) {
	t, main := ms.target, ms.target.checkout.Root
	before := ms.Procs

	// What changed under the stack. Without a record, everything might have.
	var list []stale
	switch {
	case ms.Record == nil || ms.Unknown:
		for name, s := range t.stack.Services {
			p, running := before[name]
			if len(s.Paths) > 0 && running && mayRestart(t.stack, name, p) {
				list = append(list, stale{Name: name, Files: []string{"(no record of the commit it runs)"}})
			}
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	case ms.Record.Commit != to:
		files, err := gitx.DiffNames(main, ms.Record.Commit, to)
		if err != nil {
			return false, err
		}
		list = affected(t.stack, before, sourceNames(t.stack), files)
		defer noteDaemons(t.stack, before, list, files)
	}
	// Whatever had failed gets another go: a job that failed on the last
	// update has to pass before the services that wait for it can restart,
	// even when the commit that fixes it leaves its own files as they were.
	listed := map[string]bool{}
	for _, s := range list {
		listed[s.Name] = true
	}
	for _, name := range sortedProcs(before) {
		if s := t.stack.Services[name]; s != nil && len(s.Paths) > 0 && !listed[name] && before[name].Health() == pc.Failed &&
			!isDaemon(t.stack, name) && !disabled(t.stack, name) {
			list = append(list, stale{Name: name, Files: []string{"(it had failed)"}})
		}
	}

	fmt.Println("main's stack")
	if ms.ConfigStale {
		what := configChanges(ms)
		if dry {
			fmt.Println("  its configuration would be reloaded (process-compose.yaml or hydra changed since it started): " + what)
		} else {
			again, err := reloadConfig(ms)
			if err != nil {
				return false, err
			}
			fmt.Println("  configuration reloaded (process-compose.yaml or hydra changed since it started): " + what)
			for _, name := range again {
				if !listed[name] && t.stack.Services[name] != nil {
					listed[name] = true
					list = append(list, stale{Name: name, Files: []string{"(its definition changed; it was started by hand, so it was started again)"}})
				}
			}
			// process-compose restarted the processes whose definition
			// changed. They are waited for below, like the rest.
			after, err := ms.Client.Processes()
			if err != nil {
				return false, err
			}
			for _, p := range after {
				if b, was := before[p.Name]; !listed[p.Name] && (!was || b.StartTime != p.StartTime) &&
					t.stack.Services[p.Name] != nil && mayRestart(t.stack, p.Name, p) {
					listed[p.Name] = true
					reason := "(its definition changed)"
					if !was {
						reason = "(new in the stack file)"
					}
					list = append(list, stale{Name: p.Name, Files: []string{reason}})
				}
			}
			sort.SliceStable(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		}
	}
	if len(list) == 0 {
		fmt.Println("  nothing it runs changed")
	}
	ok := restartInOrder(os.Stdout, t.stack, ms.Client, before, list, sourceNames(t.stack), dry, timeout)
	if ok && !dry {
		rec := baselineRecord{Commit: to, At: time.Now()}
		if ms.Record != nil {
			rec.Address, rec.Port = ms.Record.Address, ms.Record.Port
		} else {
			rec.Address, rec.Port = pcAddress(nil)
		}
		if err := writeBaseline(t.checkout.CommonDir, rec); err != nil {
			return false, err
		}
	}
	return ok, nil
}

// noteDaemons says when a daemon's files changed in a way nothing else
// picked up: hydra never restarts one, since it holds data.
func noteDaemons(st *config.Stack, procs map[string]pc.Process, list []stale, files []string) {
	for _, name := range st.Names() {
		s := st.Services[name]
		if p, ok := procs[name]; !ok || p.Health() == pc.Off || !isDaemon(st, name) || len(s.Paths) == 0 {
			continue
		}
		var left []string
		for _, f := range files {
			if !matches(s.Paths, f) {
				continue
			}
			taken := false
			for _, other := range list {
				taken = taken || matches(st.Services[other.Name].Paths, f)
			}
			if !taken {
				left = append(left, f)
			}
		}
		if len(left) > 0 {
			fmt.Printf("  %s was left running, since it holds data. Its files changed (%s): if that needs a restart, run `hydra process restart %s` in main.\n",
				name, someFiles(left), name)
		}
	}
}

// configChanges says what a reload changes: which processes are added,
// dropped or defined differently, and which ones it switches on. It
// compares the file the stack was started from with a fresh render.
func configChanges(ms *mainState) string {
	type doc struct {
		Processes map[string]map[string]any `yaml:"processes"`
	}
	var old, fresh doc
	have, _ := os.ReadFile(renderedPath(ms.target.checkout.Root))
	want, err := ms.target.render(plan.BaselinePlan(ms.target.stack), "")
	if err != nil || yaml.Unmarshal(have, &old) != nil || yaml.Unmarshal(want, &fresh) != nil {
		return "(can't compare the two files)"
	}
	var added, dropped, changed, on []string
	for name, p := range fresh.Processes {
		o, was := old.Processes[name]
		switch {
		case !was:
			added = append(added, name)
		case !reflect.DeepEqual(o, p):
			changed = append(changed, name)
		}
		if was && o["disabled"] == true && p["disabled"] != true {
			on = append(on, name)
		}
	}
	for name := range old.Processes {
		if _, still := fresh.Processes[name]; !still {
			dropped = append(dropped, name)
		}
	}
	var parts []string
	for _, g := range []struct {
		verb  string
		names []string
	}{{"changes", changed}, {"adds", added}, {"drops", dropped}, {"SWITCHES ON", on}} {
		if len(g.names) > 0 {
			sort.Strings(g.names)
			parts = append(parts, g.verb+" "+strings.Join(g.names, ", "))
		}
	}
	if len(parts) == 0 {
		return "no process is defined differently"
	}
	return strings.Join(parts, "; ")
}

// reloadConfig renders the main checkout's file again and hands it to the
// running process-compose, which restarts only the processes whose
// definition changed. A process someone started by hand (a disabled one)
// is started again if the reload stopped it; those are returned.
func reloadConfig(ms *mainState) (startedAgain []string, err error) {
	t := ms.target
	data, err := t.render(plan.BaselinePlan(t.stack), "")
	if err != nil {
		return nil, err
	}
	out := renderedPath(t.checkout.Root)
	current, _ := os.ReadFile(out)
	host, port := pcAddress(nil)
	if ms.Record != nil && ms.Record.Port != 0 {
		host, port = ms.Record.Address, ms.Record.Port
	}
	// In steps, so a process the new file adds starts after what it waits
	// for (see applyProject).
	err = applyProject(out, current, data, func(file string) error {
		cmd := exec.Command("process-compose", "project", "update", "-f", file, "--address", host, "--port", fmt.Sprint(port))
		cmd.Dir = t.checkout.Root
		if msg, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("process-compose project update: %v\n%s", err, bytes.TrimSpace(msg))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Now what runs is this file's: the router moves with it.
	if err := writeFile(startedFromPath(t.checkout.Root), t.stack.Source); err != nil {
		return nil, err
	}
	time.Sleep(500 * time.Millisecond)
	after, err := ms.Client.Processes()
	if err != nil {
		return nil, err
	}
	for _, p := range after {
		if b, ok := ms.Procs[p.Name]; ok && b.IsRunning && !p.IsRunning && p.Health() == pc.Off {
			if err := ms.Client.Start(p.Name); err != nil {
				return startedAgain, fmt.Errorf("starting %s again after the reload: %w", p.Name, err)
			}
			startedAgain = append(startedAgain, p.Name)
		}
	}
	return startedAgain, nil
}

// restartInOrder restarts each listed process after the listed ones it
// depends on, waiting for each to finish (a job) or come up (a service)
// before the next. It stops at the first failure: a service must not start
// on migrations that didn't apply.
func restartInOrder(out io.Writer, st *config.Stack, c *pc.Client, before map[string]pc.Process, list []stale, sources map[string]string, dry bool, timeout time.Duration) bool {
	if len(list) == 0 {
		return true
	}
	in := map[string]stale{}
	for _, s := range list {
		in[s.Name] = s
	}
	var order []string
	done := map[string]bool{}
	for len(order) < len(list) {
		progressed := false
		for _, s := range list { // the first one whose listed dependencies are done
			if done[s.Name] {
				continue
			}
			ready := true
			for _, d := range st.Services[sources[s.Name]].DependsOn {
				if _, listed := in[d]; listed && !done[d] {
					ready = false
				}
			}
			if ready {
				order, done[s.Name], progressed = append(order, s.Name), true, true
				break
			}
		}
		if !progressed { // a dependency cycle: take the rest as they come
			for _, s := range list {
				if !done[s.Name] {
					order, done[s.Name] = append(order, s.Name), true
				}
			}
		}
	}

	// On a terminal, say what is being waited for; the line is replaced by
	// the result.
	live := false
	if f, ok := out.(*os.File); ok {
		if info, err := f.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			live = true
		}
	}
	// The columns are measured first, so that each line can be printed the
	// moment its step ends: an update takes minutes, and a table that only
	// appears at the end looks like one that hangs.
	type step struct {
		verb, would, why string
		job, again       bool
	}
	steps := map[string]step{}
	wide := func(a int, s string) int { return max(a, utf8.RuneCountInString(s)) }
	wName, wVerb, wWould, wWhy := 0, len("FAILED"), 0, 0
	for _, name := range order {
		b := before[name]
		s := step{verb: "restarted", would: "would be restarted", why: oneFile(in[name].Files), job: isJob(st, sources[name], b)}
		if s.job {
			s.verb, s.would = "ran again", "would run again"
		}
		if _, was := before[name]; !was {
			s.verb, s.would = "started", "would be started" // new in the stack file
		}
		// The reload may have restarted it already, before a job it waits
		// for ran again here: then it has to start once more, after the job.
		for _, d := range st.Services[sources[name]].DependsOn {
			if _, listed := in[d]; listed {
				s.again = true
			}
		}
		steps[name] = s
		wName, wVerb, wWould, wWhy = wide(wName, name), wide(wVerb, s.verb), wide(wWould, s.would), wide(wWhy, s.why)
	}
	for i, name := range order {
		s := steps[name]
		if dry {
			fmt.Fprintf(out, "  %-*s  %-*s  %s\n", wName, name, wWould, s.would, s.why)
			continue
		}
		if live {
			fmt.Fprintf(out, "  %s: %s…", name, map[bool]string{true: "running again", false: "restarting"}[s.job])
		}
		start := time.Now()
		p, err := restartAndWait(st, sources[name], c, name, before[name], s.job, s.again, timeout)
		took := time.Since(start).Round(time.Second)
		if live {
			fmt.Fprint(out, "\r\033[K")
		}
		if err != nil {
			fmt.Fprintf(out, "  %-*s  %-*s  %v, after %s\n", wName, name, wVerb, "FAILED", err, took)
			if tail, _ := logTail(c, name); tail != "" {
				fmt.Fprint(out, "   "+tail+"\n")
			}
			if rest := order[i+1:]; len(rest) > 0 {
				fmt.Fprintf(out, "  not restarted, because %s failed: %s\n", name, strings.Join(rest, ", "))
			}
			return false
		}
		fmt.Fprintf(out, "  %-*s  %-*s  %-*s  %s in %s\n", wName, name, wVerb, s.verb, wWhy, s.why, p.Describe(), took)
	}
	return true
}

// isJob tells a process that runs and exits (pnpm install, migrations) from
// a service: another process waits for it to complete, or it had exited
// cleanly when hydra looked and its definition has nothing that says when
// it is ready.
func isJob(st *config.Stack, source string, before pc.Process) bool {
	procs, _ := st.Raw["processes"].(map[string]any)
	for _, other := range procs {
		o, _ := other.(map[string]any)
		deps, _ := o["depends_on"].(map[string]any)
		dep, _ := deps[source].(map[string]any)
		if cond, _ := dep["condition"].(string); strings.HasPrefix(cond, "process_completed") {
			return true
		}
	}
	proc, _ := procs[source].(map[string]any)
	_, probe := proc["readiness_probe"]
	_, line := proc["ready_log_line"]
	return before.Health() == pc.Done && !probe && !line
}

// restartAndWait restarts a process and waits until its new run has finished
// (a job) or is up (a service). A process that something already restarted
// since the snapshot in before is only waited for, unless always is set.
func restartAndWait(st *config.Stack, source string, c *pc.Client, name string, before pc.Process, job, always bool, timeout time.Duration) (pc.Process, error) {
	if now, err := c.Process(name); err != nil {
		return now, err
	} else if always || now.StartTime == before.StartTime {
		before = now // wait for the run that starts now
		if err := c.Restart(name); err != nil {
			return now, err
		}
	}
	deadline := time.Now().Add(timeout)
	var upSince time.Time
	for {
		p, err := c.Process(name)
		p = settle(st, source, p)
		if err == nil && p.StartTime != before.StartTime {
			switch p.Health() {
			case pc.Done:
				if !job {
					return p, errors.New("exited right away (code 0), though it should keep running")
				}
				return p, nil
			case pc.Failed:
				return p, errors.New(p.Describe())
			case pc.Up:
				if job {
					break // still running
				}
				if p.IsReady == "Ready" {
					return p, nil
				}
				// Nothing says when it is ready: up for a few seconds will do.
				if upSince.IsZero() {
					upSince = time.Now()
				} else if time.Since(upSince) > 3*time.Second {
					return p, nil
				}
			case pc.Off:
				upSince = time.Time{}
				if p.Status == "Skipped" {
					// process-compose gave up on it: something it waits for
					// failed or was stopped.
					return p, errors.New("not started: a process it depends on failed or was stopped")
				}
				// A process that is off by default reads "Disabled", not
				// "Pending", until its new run begins: process-compose is
				// waiting for what it depends on. Wait with it.
			default:
				upSince = time.Time{}
			}
		}
		if time.Now().After(deadline) {
			if err == nil && p.Health() == pc.Off {
				return p, fmt.Errorf("not started after %s: process-compose is still waiting for a process it depends on", timeout)
			}
			return p, fmt.Errorf("still %s after %s", strings.ToLower(p.Status), timeout)
		}
		time.Sleep(400 * time.Millisecond)
	}
}

// wtUpdate is what an update does to one worktree.
type wtUpdate struct {
	root, name string
	parent     string   // the ref merged in: main, or the branch it is stacked on
	depth      int      // position in its stack: parents are updated first
	action     string   // "merge", "fast-forward", or "" when left alone
	note       string   // why it was left alone, or what happened
	hand       bool     // someone has to step in
	detail     []string // for the ones that need a hand: files, and the command
}

// updateWorktrees merges main (or the branch a worktree is stacked on) into
// every worktree that is behind it (only the one at only, when that is
// set), and returns how many need a hand.
func updateWorktrees(co gitx.Checkout, ms *mainState, mainCommit string, dry bool, timeout time.Duration, only string) (int, error) {
	main, base := ms.target.checkout.Root, ms.target.stack.Hydra.BaseRef
	wts, err := stack.List(main)
	if err != nil {
		return 0, err
	}
	var ups []*wtUpdate
	for _, wt := range wts {
		if wt.Root == main || (only != "" && wt.Root != only) {
			continue
		}
		ups = append(ups, &wtUpdate{root: wt.Root, name: wt.Branch})
	}
	if len(ups) == 0 {
		return 0, nil
	}
	for _, u := range ups {
		u.inspect(ms, base)
	}
	sort.SliceStable(ups, func(i, j int) bool { return ups[i].depth < ups[j].depth })

	fmt.Println("\nworktrees")
	// One line per worktree, as each is done: a merge that restarts a
	// worktree's stack takes a while.
	branch := func(u *wtUpdate) string {
		if u.name == "" {
			return "(detached)"
		}
		return shortBranch(u.name)
	}
	wRoot, wBranch := 0, 0
	for _, u := range ups {
		wRoot = max(wRoot, utf8.RuneCountInString(filepath.Base(u.root)))
		wBranch = max(wBranch, utf8.RuneCountInString(branch(u)))
	}
	hand := 0
	for _, u := range ups {
		// A parent merged a moment ago may have changed what this needs.
		if u.depth > 1 && !dry {
			u.action, u.note, u.hand = "", "", false
			u.inspect(ms, base)
		}
		u.decide(main, base, mainCommit, dry)
		if u.action != "" && !dry {
			u.apply(timeout)
		}
		if u.hand {
			hand++
		}
		fmt.Printf("  %-*s  %-*s  %s\n", wRoot, filepath.Base(u.root), wBranch, branch(u), u.note)
	}
	for _, u := range ups {
		if len(u.detail) > 0 {
			fmt.Printf("\n  %s\n", u.root)
			for _, line := range u.detail {
				fmt.Println("    " + line)
			}
		}
	}
	return hand, nil
}

// inspect finds what the worktree is stacked on.
func (u *wtUpdate) inspect(ms *mainState, base string) {
	u.parent, u.depth = base, 1
	if u.name == "" {
		return
	}
	chain, _, err := stack.Chain(u.root, stack.Options{
		BaseRef: base,
		ParentOverride: func(root string) string {
			lf, _ := readLocal(filepath.Join(root, ".hydra.local.yaml"))
			if lf == nil {
				return ""
			}
			return lf.Parent
		},
	})
	if err == nil && len(chain) > 0 {
		u.parent, u.depth = chain[len(chain)-1].Parent, len(chain)
	}
}

// decide works out what to do, without changing anything: git merge-tree
// tries the merge in memory.
func (u *wtUpdate) decide(main, base, mainCommit string, dry bool) {
	if u.name == "" {
		u.note = "left alone: no branch is checked out there"
		return
	}
	if _, err := os.Stat(u.root); err != nil {
		u.note = "left alone: its directory is gone (`git worktree prune` forgets it)"
		return
	}
	// In a dry run main hasn't moved yet: merge what it will be.
	theirs := u.parent
	if u.parent == base {
		theirs = mainCommit
	}
	if gitx.IsAncestor(u.root, theirs, "HEAD") {
		u.note = "up to date"
		if u.parent != base {
			u.note = "up to date with " + shortBranch(u.parent) + ", which it is stacked on"
		}
		return
	}
	behind, _ := gitOut(u.root, "rev-list", "--count", "HEAD.."+theirs)
	gitDir, _ := gitOut(u.root, "rev-parse", "--absolute-git-dir")
	for _, f := range []string{"MERGE_HEAD", "rebase-merge", "rebase-apply", "CHERRY_PICK_HEAD"} {
		if _, err := os.Stat(filepath.Join(gitDir, f)); err == nil {
			u.note, u.hand = fmt.Sprintf("left alone, %s behind %s: a merge or rebase is in progress there", commits(behind), u.parent), true
			return
		}
	}
	if dirty, _ := gitOut(u.root, "status", "--porcelain", "--untracked-files=no"); dirty != "" {
		n := len(strings.Split(dirty, "\n"))
		u.note, u.hand = fmt.Sprintf("left alone, %s behind %s: %d file%s with uncommitted changes", commits(behind), u.parent, n, plural(n)), true
		return
	}
	would := map[bool]string{true: "would be ", false: ""}[dry]
	if gitx.IsAncestor(u.root, "HEAD", theirs) {
		u.action, u.note = "fast-forward", fmt.Sprintf("%sfast-forwarded to %s (%s; it has none of its own)", would, u.parent, commits(behind))
		return
	}
	tree, files, err := mergeTree(u.root, theirs, "HEAD")
	switch {
	case err != nil:
		u.note, u.hand = fmt.Sprintf("left alone: %v", err), true
	case len(files) == 0:
		if t, _ := gitOut(u.root, "rev-parse", theirs+"^{tree}"); tree == t {
			u.note = fmt.Sprintf("left alone: everything on it is already in %s (merged upstream?); it can be removed", u.parent)
			return
		}
		u.action, u.note = "merge", fmt.Sprintf("%s %smerged in (%s)", u.parent, would, commits(behind))
	default:
		u.note, u.hand = fmt.Sprintf("CONFLICTS with %s in %d file%s (%s behind)", u.parent, len(files), plural(len(files)), commits(behind)), true
		const show = 4
		for i, f := range files {
			if i == show && len(files) > show+1 {
				u.detail = append(u.detail, fmt.Sprintf("conflict: … and %d more", len(files)-show))
				break
			}
			u.detail = append(u.detail, "conflict: "+f)
		}
		u.detail = append(u.detail, fmt.Sprintf("resolve:  git -C %s merge %s", shQuote(u.root), u.parent))
	}
}

// apply does the merge decide chose, then restarts what the merged commits
// touched in the worktree's running stack.
func (u *wtUpdate) apply(timeout time.Duration) {
	before, err := gitx.Head(u.root)
	if err != nil {
		u.note, u.hand = "left alone: "+err.Error(), true
		return
	}
	args := []string{"-C", u.root, "merge", "--no-edit", "--quiet"}
	if u.action == "fast-forward" {
		args = append(args, "--ff-only")
	}
	cmd := exec.Command("git", append(args, u.parent)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if msg, err := cmd.CombinedOutput(); err != nil {
		exec.Command("git", "-C", u.root, "merge", "--abort").Run()
		first, _, _ := strings.Cut(strings.TrimSpace(string(msg)), "\n")
		u.note, u.hand = fmt.Sprintf("NOT merged: git merge %s failed (%s); the worktree is as it was", u.parent, first), true
		return
	}
	after, _ := gitx.Head(u.root)
	if restarted := restartInWorktree(u.root, before, after, timeout); restarted != "" {
		u.note += "; " + restarted
	}
}

// restartInWorktree restarts, in a worktree whose stack is running, the
// processes whose files a merge changed: pnpm install has to run again
// before the dev server that needs the new packages.
func restartInWorktree(root, before, after string, timeout time.Duration) string {
	wco, err := gitx.Find(root)
	if err != nil {
		return ""
	}
	ws, err := loadWorktree(wco)
	if err != nil || !ws.Up {
		return ""
	}
	files, err := gitx.DiffNames(root, before, after)
	if err != nil {
		return ""
	}
	sources := map[string]string{}
	for _, row := range ws.Plan.Services {
		if row.Local && !row.Disabled {
			sources[row.Name] = row.Source
		}
	}
	list := affected(ws.target.stack, ws.Procs, sources, files)
	if len(list) == 0 {
		return ""
	}
	var names []string
	for _, s := range list {
		names = append(names, s.Name)
	}
	// Quietly: the caller prints one line per worktree.
	if !restartInOrder(io.Discard, ws.target.stack, ws.Client, ws.Procs, list, sources, false, timeout) {
		return "its stack did NOT come back after restarting " + strings.Join(names, ", ") + " (run `hydra status` there)"
	}
	return "restarted there: " + strings.Join(names, ", ")
}

// withoutCLIEdit is main's uncommitted files less supabase/.gitignore when
// the Supabase CLI's lines are all that changed there: an update puts the
// file back (a dry run only says it would) instead of refusing to pull.
func withoutCLIEdit(ms *mainState, dry bool) []string {
	root := ms.target.checkout.Root
	if !contains(ms.Dirty, "supabase/.gitignore") || !hasKind(ms.target.stack, config.KindSupabase) {
		return ms.Dirty
	}
	if _, ok, err := cliEdited(root); err != nil || !ok {
		return ms.Dirty
	}
	if dry {
		fmt.Println("supabase/.gitignore in main has only the Supabase CLI's lines added: it would be put back")
	} else {
		if _, err := tidySupabase(ms.target.checkout); err != nil {
			return ms.Dirty
		}
		fmt.Println("put supabase/.gitignore in main back: the Supabase CLI had appended its lock files to it")
	}
	var rest []string
	for _, f := range ms.Dirty {
		if f != "supabase/.gitignore" {
			rest = append(rest, f)
		}
	}
	return rest
}
