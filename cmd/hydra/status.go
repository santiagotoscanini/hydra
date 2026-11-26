package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/pc"
	"github.com/santiagotoscanini/hydra/internal/slots"
	"github.com/santiagotoscanini/hydra/internal/stack"
)

// errProblems ends the command with status 1 after the problems were
// printed; there is nothing more to say.
type errProblems struct{}

func (errProblems) Error() string { return "" }

// statusCmd answers "can I trust what runs here?": the plan, checked
// against what the two process-compose servers report and against git.
//
//	hydra status         this worktree (in the main checkout: everything)
//	hydra status --all   the main checkout and every worktree
//	hydra status --json  all of it as JSON (--watch: again every second)
func statusCmd(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	all := fs.Bool("all", false, "")
	wait := fs.Bool("wait", false, "")
	timeout := fs.Duration("timeout", 10*time.Minute, "")
	asJSON := fs.Bool("json", false, "")
	watch := fs.Bool("watch", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	co, err := gitx.Find(".")
	if err != nil {
		return err
	}
	if *asJSON {
		return statusJSON(co, *watch)
	}
	waited := time.Duration(0) // how long --wait waited before it gave up
	if *wait {
		gaveUp, err := waitSettled(co, co.IsMain() || *all, *timeout)
		if err != nil {
			return err
		}
		if gaveUp {
			waited = *timeout
		}
	}
	ms, err := loadMain(co)
	if err != nil {
		return err
	}
	if co.IsMain() || *all {
		return statusAll(co, ms, waited)
	}
	ws, err := loadWorktree(co)
	if err != nil {
		return err
	}
	ws.Far = farBehindNow(ms, ws)
	printMainLine(ms)
	down := "stack not started"
	if n := readIdleNote(ws.target.checkout.Root); n != nil {
		down = "stack stopped after " + n.After + " idle"
	}
	fmt.Printf("worktree  %s  %s  %s\n", ws.target.checkout.Root, slotText(ws), upText(ws.Up, "stack up", down))
	printAddresses(ws.target.stack, config.HostLabel(ws.target.checkout.Root), "")
	fmt.Println()
	printWorktreeTable(ms, ws)

	problems := stillStarting(worktreeProblems(ms, ws, true), waited)
	more, err := chainProblems(ms, ws.target.checkout.Root)
	if err != nil {
		return err
	}
	problems = append(problems, more...)
	want := map[int]portUse{}
	worktreePorts(want, ws)
	problems = append(problems, strayProblems(findStrays(want))...)
	fmt.Println()
	if len(problems) == 0 {
		fmt.Println("ok: everything this worktree uses is up and in step with main.")
		return nil
	}
	printProblems(os.Stdout, problems)
	if n := len(blocking(problems)); n > 0 {
		return errProblems{}
	}
	fmt.Println("\nok, with the notes above.")
	return nil
}

// stillStarting turns what is still starting into a problem once --wait has
// given up on it: a start that takes longer than the wait isn't "ok".
func stillStarting(problems []problem, waited time.Duration) []problem {
	if waited == 0 {
		return problems
	}
	for i, p := range problems {
		if p.Passing {
			first, rest, more := strings.Cut(p.Text, "\n")
			first = strings.TrimSuffix(first, ".") + fmt.Sprintf(", after waiting %s.", waited)
			if more {
				first += "\n" + rest
			}
			problems[i].Text, problems[i].Blocks = first, true
		}
	}
	return problems
}

// looksEvery is how often a wait reads everything again: git and the plan,
// which cost a second of CPU in a large repository. In between it only asks
// the two process-compose servers what runs.
var looksEvery = 10 * time.Second

// waitSettled returns once nothing is still starting: everything is up, or
// something failed and stayed failed for a few seconds (a process that is
// between restarts looks failed for a moment). The caller then prints the
// state as usual. gaveUp says that the time ran out first.
func waitSettled(co gitx.Checkout, mainOnly bool, timeout time.Duration) (gaveUp bool, err error) {
	deadline := time.Now().Add(timeout)
	live := false
	if info, err := os.Stderr.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		live = true
	}
	defer func() {
		if live {
			fmt.Fprint(os.Stderr, "\r\033[K")
		}
	}()
	var (
		ms     *mainState
		ws     *wtState
		looked time.Time
	)
	failedFor := 0
	for {
		fresh := ms == nil || time.Since(looked) >= looksEvery
		if fresh {
			if ms, err = loadMain(co); err != nil {
				return false, err
			}
			if !mainOnly {
				if ws, err = loadWorktree(co); err != nil {
					return false, err
				}
			}
			looked = time.Now()
		} else {
			if err := ms.refresh(); err != nil {
				return false, err
			}
			if ws != nil {
				if err := ws.refresh(); err != nil {
					return false, err
				}
			}
		}
		var problems []problem
		if mainOnly {
			problems = mainProblems(ms)
			if ms.Up {
				problems = append(problems, failing(ms, nil)...)
			}
		} else {
			problems = worktreeProblems(ms, ws, true)
		}
		var passing []string
		for _, p := range problems {
			if p.Passing {
				first, _, _ := strings.Cut(p.Text, "\n")
				passing = append(passing, first)
			}
		}
		if len(blocking(problems)) > 0 {
			failedFor++
		} else {
			failedFor = 0
		}
		if failedFor >= 4 || (failedFor == 0 && len(passing) == 0) {
			if !fresh {
				// Settled, going by a plan that is a few seconds old: an edit
				// since then may have added a service. Look again first.
				ms = nil
				continue
			}
			return false, nil
		}
		if time.Now().After(deadline) {
			return len(passing) > 0, nil
		}
		if live && len(passing) > 0 {
			fmt.Fprintf(os.Stderr, "\r\033[Kwaiting: %s", passing[0])
		}
		time.Sleep(time.Second)
	}
}

func slotText(ws *wtState) string {
	if ws.Slot == 0 {
		return ws.Name + ", no slot yet"
	}
	return fmt.Sprintf("%s, slot %d", ws.Name, ws.Slot)
}

func upText(up bool, yes, no string) string {
	if up {
		return yes
	}
	return no
}

func printMainLine(ms *mainState) {
	at := "on " + ms.Branch
	if ms.Branch == "" {
		at = "detached"
	}
	fmt.Printf("main      %s  %s at %s  %s\n", ms.target.checkout.Root, at, gitx.Short(ms.Head), upText(ms.Up, "stack up", "stack DOWN"))
	if ms.Behind > 0 {
		fmt.Printf("          %d commit%s behind its upstream; `hydra update` pulls them\n", ms.Behind, plural(ms.Behind))
	}
}

// printAddresses lists the name-based address of each service with a hosts
// port, in the checkout whose name is label ("" for main).
func printAddresses(st *config.Stack, label, more string) {
	if parts := addresses(st, label); len(parts) > 0 {
		fmt.Printf("address   %s%s\n", strings.Join(parts, ", "), more)
	}
}

// addresses is "web http://fla-709.localhost:3100" for each service with a
// hosts port, in the checkout whose name is label ("" for main).
func addresses(st *config.Stack, label string) []string {
	var out []string
	for _, name := range st.Names() {
		if u := st.HostsURL(st.Services[name], label); u != "" {
			out = append(out, name+" "+u)
		}
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// printWorktreeTable lists what runs here and what this worktree takes from
// main, each with its state as process-compose reports it right now.
func printWorktreeTable(ms *mainState, ws *wtState) {
	used := uses(ws.target.stack, ws.Plan)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERVICE\tRUNS\tPORT\tSTATE")
	hidden := 0
	for _, row := range ws.Plan.Services {
		port := "-"
		if row.Port != 0 {
			port = strconv.Itoa(row.Port)
		}
		switch {
		case row.Local:
			state := "not started"
			if p, ok := ws.Procs[row.Name]; ok {
				state = p.Describe()
			} else if ws.Up {
				state = "not in the project yet"
			}
			where := "here"
			if row.Disabled {
				where = "here (off)"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", row.Name, where, port, state)
		case row.Routed:
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", row.Name, "main, routed", port, mainProcState(ms, row.Name))
		default:
			if _, ok := used[row.Name]; !ok {
				hidden++
				continue
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", row.Name, "main", port, mainProcState(ms, row.Name))
		}
	}
	w.Flush()
	if hidden > 0 {
		fmt.Printf("(%d more of main's services aren't used by anything here; `hydra plan` lists them)\n", hidden)
	}
}

func mainProcState(ms *mainState, name string) string {
	if !ms.Up {
		return "main is down"
	}
	p, ok := ms.Procs[name]
	if !ok {
		return "not in main's project"
	}
	return p.Describe()
}

// statusAll prints the main checkout's stack and one line per worktree.
func statusAll(co gitx.Checkout, ms *mainState, waited time.Duration) error {
	printMainLine(ms)
	printAddresses(ms.target.stack, "", "; a worktree's: <worktree>.localhost")
	bad := false
	if ms.Up {
		fmt.Println()
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "SERVICE\tSTATE")
		off := 0
		for _, name := range sortedProcs(ms.Procs) {
			p := ms.Procs[name]
			if p.Health() == pc.Off {
				off++
				continue
			}
			fmt.Fprintf(w, "%s\t%s\n", name, p.Describe())
		}
		w.Flush()
		if off > 0 {
			fmt.Printf("(%d more are off)\n", off)
		}
	}

	wts, err := stack.List(co.Main)
	if err != nil {
		return err
	}
	type line struct {
		ws       *wtState
		root     string
		err      error
		problems []problem
		counts   string
	}
	var lines []*line
	for _, wt := range wts {
		if wt.Root != ms.target.checkout.Root {
			lines = append(lines, &line{root: wt.Root})
		}
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, l := range lines {
		wg.Add(1)
		go func(l *line) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			wco, err := gitx.Find(l.root)
			if err != nil {
				l.err = err
				return
			}
			if l.ws, l.err = loadWorktree(wco); l.err != nil {
				return
			}
			l.ws.Far = farBehindNow(ms, l.ws)
			l.problems = worktreeProblems(ms, l.ws, l.ws.Up)
			l.counts = behindAhead(l.root, ms.Head)
		}(l)
	}
	wg.Wait()

	if len(lines) > 0 {
		fmt.Println()
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "WORKTREE\tBRANCH\tSLOT\tSTACK\tRUNS HERE\tVS MAIN")
		for _, l := range lines {
			name := filepath.Base(l.root)
			if l.err != nil {
				fmt.Fprintf(w, "%s\t-\t-\t-\t-\t%v\n", name, l.err)
				continue
			}
			ws := l.ws
			slot := "-"
			if ws.Slot != 0 {
				slot = strconv.Itoa(ws.Slot)
			}
			here := strings.Join(enabledLocal(ws), ", ")
			if here == "" {
				here = "-"
			}
			vs := "in step"
			if len(ws.Plan.OutOfStep) > 0 {
				var names []string
				for _, k := range ws.Plan.OutOfStep {
					names = append(names, k.Service)
				}
				vs = "behind main in " + strings.Join(names, ", ")
				if ws.Plan.MainBehind {
					vs = "main is behind it in " + strings.Join(names, ", ")
				}
			}
			if l.counts != "" {
				vs += "  (" + l.counts + ")"
			}
			if f := ws.Far; f != nil {
				vs = "! " + vs
				if len(f.Conflicts) > 0 {
					vs += fmt.Sprintf(", merging main conflicts in %d file%s", len(f.Conflicts), plural(len(f.Conflicts)))
				}
			}
			state := upText(ws.Up, "up", "-")
			if n := readIdleNote(l.root); n != nil && !ws.Up {
				state = "idle-stopped"
			}
			if ws.Up {
				if n := len(blocking(l.problems)); n > 0 {
					state = fmt.Sprintf("up, %d problem%s", n, plural(n))
					bad = true
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", name, shortBranch(ws.Name), slot, state, here, vs)
		}
		w.Flush()
	}

	problems := mainProblems(ms)
	if ms.Up {
		problems = append(problems, failing(ms, nil)...)
	}
	if err := printBareTable(ms, wts); err != nil {
		return err
	}
	problems = stillStarting(append(problems, goneProblems(co)...), waited)
	want := map[int]portUse{}
	mainPorts(want, ms)
	problems = append(problems, strayProblems(findStrays(want))...)
	fmt.Println()
	if len(problems) > 0 {
		printProblems(os.Stdout, problems)
	}
	if len(blocking(problems)) > 0 || bad {
		if bad {
			fmt.Println("Run `hydra status` in a worktree marked above for its problems.")
		}
		return errProblems{}
	}
	if len(problems) == 0 {
		fmt.Println("ok: main's stack is up and runs what main has checked out.")
	} else {
		fmt.Println("\nok, with the notes above.")
	}
	return nil
}

// printBareTable lists the stack branches no worktree has checked out, and
// how each stands against its parent. Like a worktree behind main, one
// behind its parent is said here, not as a problem: `hydra status` in a
// worktree stacked on it warns.
func printBareTable(ms *mainState, wts []stack.Worktree) error {
	main, base := ms.target.checkout.Root, ms.target.stack.Hydra.BaseRef
	view, err := loadStacks(main, base, wts)
	if err != nil {
		return err
	}
	rows := bareRows(view, main, base, ms.Head, nil)
	if len(rows) == 0 {
		return nil
	}
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NO CHECKOUT\tON\tVS ITS PARENT\tUNDER")
	chains := view.chains()
	for _, r := range rows {
		vs := "in step"
		switch {
		case strings.HasPrefix(r.u.note, "CONFLICTS"):
			vs = fmt.Sprintf("! %s behind, merging it conflicts", commits(strconv.Itoa(r.u.behind)))
		case r.u.hand:
			vs = "! " + r.u.note
		case r.u.behind > 0:
			vs = "! " + commits(strconv.Itoa(r.u.behind)) + " behind"
		case strings.HasPrefix(r.u.note, "left alone: everything"):
			vs = "landed: everything on it is in " + shortBranch(r.u.parent)
		}
		under := strings.Join(stackedOn(chains, r.name), ", ")
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", shortBranch(r.name), shortBranch(r.u.parent), vs, under)
	}
	w.Flush()
	if hint := bareHint(rows); hint != "" {
		fmt.Println(hint)
	}
	return nil
}

// bareHint says what to run for the branches with no checkout that are
// behind, if any is.
func bareHint(rows []bareRow) string {
	for _, r := range rows {
		if r.u.action != "" {
			return "(`hydra update` merges them without a checkout, parents first; `hydra update --branch NAME`, one.)"
		}
	}
	return ""
}

// chainProblems warns about the branches with no checkout in this
// worktree's stack that are behind their parents: it runs their code.
func chainProblems(ms *mainState, root string) ([]problem, error) {
	main, base := ms.target.checkout.Root, ms.target.stack.Hydra.BaseRef
	wts, err := stack.List(main)
	if err != nil {
		return nil, err
	}
	view, err := loadStacks(main, base, wts)
	if err != nil {
		return nil, err
	}
	if len(view.st.Bare) == 0 {
		return nil, nil
	}
	_, chain, err := view.place(root, base)
	if err != nil || len(chain) < 2 {
		return nil, nil
	}
	var problems []problem
	for _, r := range bareRows(view, main, base, ms.Head, chain[:len(chain)-1]) {
		if p := r.problem([]string{"this worktree"}); p != nil {
			problems = append(problems, *p)
		}
	}
	return problems, nil
}

// goneProblems names the worktrees that were removed with their stack still
// up: memory and ports in use for nothing. The stack's own watcher stops it
// within seconds, so this shows when that didn't work. (A removed worktree
// with nothing running only holds a slot, until the next start frees it;
// `hydra slots` lists those.)
func goneProblems(co gitx.Checkout) []problem {
	gone, _ := slots.Gone(co.CommonDir)
	var out []problem
	for _, e := range gone {
		if pc.Unix(socketFor(e.Root)).Alive() {
			out = append(out, problem{
				Text: fmt.Sprintf("%s was removed, but its stack is still running (slot %d). The next `hydra up` in any checkout stops it.", e.Root, e.Slot),
				Fix:  "hydra slots free " + shQuote(e.Root),
			})
		}
	}
	return out
}

// enabledLocal lists the services a worktree runs, leaving out the ones
// that are switched off until someone starts them.
func enabledLocal(ws *wtState) []string {
	var out []string
	for _, row := range ws.Plan.Services {
		if row.Local && !row.Disabled {
			out = append(out, row.Name)
		}
	}
	return out
}

// farBehindNow is farBehindOf for a worktree that was just loaded.
func farBehindNow(ms *mainState, ws *wtState) *farBehind {
	root := ws.target.checkout.Root
	out, err := gitOut(root, "rev-list", "--count", "HEAD.."+ms.Head)
	if err != nil {
		return nil
	}
	behind, _ := strconv.Atoi(strings.TrimSpace(out))
	head, err := gitx.Head(root)
	if err != nil {
		return nil
	}
	return farBehindOf(root, head, ms.Head, behind, ws.target.stack.Hydra.BehindWarn)
}

// behindAhead counts the commits main has that the worktree lacks, and the
// other way round: "343 behind, 7 ahead".
func behindAhead(root, mainHead string) string {
	out, err := gitOut(root, "rev-list", "--left-right", "--count", mainHead+"...HEAD")
	if err != nil {
		return ""
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return ""
	}
	var parts []string
	if f[0] != "0" {
		parts = append(parts, f[0]+" behind")
	}
	if f[1] != "0" {
		parts = append(parts, f[1]+" ahead")
	}
	return strings.Join(parts, ", ")
}
