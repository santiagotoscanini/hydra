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

	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/pc"
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
func statusCmd(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	all := fs.Bool("all", false, "")
	wait := fs.Bool("wait", false, "")
	timeout := fs.Duration("timeout", 10*time.Minute, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	co, err := gitx.Find(".")
	if err != nil {
		return err
	}
	if *wait {
		if err := waitSettled(co, co.IsMain() || *all, *timeout); err != nil {
			return err
		}
	}
	ms, err := loadMain(co)
	if err != nil {
		return err
	}
	if co.IsMain() || *all {
		return statusAll(co, ms)
	}
	ws, err := loadWorktree(co)
	if err != nil {
		return err
	}
	printMainLine(ms)
	fmt.Printf("worktree  %s  %s  %s\n", ws.target.checkout.Root, slotText(ws), upText(ws.Up, "stack up", "stack not started"))
	fmt.Println()
	printWorktreeTable(ms, ws)

	problems := worktreeProblems(ms, ws, true)
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

// waitSettled returns once nothing is still starting: everything is up, or
// something failed and stayed failed for a few seconds (a process that is
// between restarts looks failed for a moment). The caller then prints the
// state as usual.
func waitSettled(co gitx.Checkout, mainOnly bool, timeout time.Duration) error {
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
	failedFor := 0
	for {
		ms, err := loadMain(co)
		if err != nil {
			return err
		}
		var problems []problem
		if mainOnly {
			problems = mainProblems(ms)
			if ms.Up {
				problems = append(problems, failing(ms, nil)...)
			}
		} else {
			ws, err := loadWorktree(co)
			if err != nil {
				return err
			}
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
		if failedFor >= 4 || (failedFor == 0 && len(passing) == 0) || time.Now().After(deadline) {
			return nil
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
func statusAll(co gitx.Checkout, ms *mainState) error {
	printMainLine(ms)
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
			state := upText(ws.Up, "up", "-")
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
