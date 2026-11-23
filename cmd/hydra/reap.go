package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/pc"
	"github.com/santiagotoscanini/hydra/internal/routes"
	"github.com/santiagotoscanini/hydra/internal/slots"
)

// A worktree that is removed while its stack runs leaves the stack behind:
// process-compose and the services keep running with no directory under
// them, on ports the slot would hand to the next worktree, and its own
// Supabase stack keeps its containers and data. Nothing tells hydra when a
// worktree is removed (git has no hook for it, and tools that manage
// worktrees delete the directory), so hydra cleans up when it next looks:
//
//   - the worktree's own watcher notices within seconds and stops the stack
//     it is part of (stopGone);
//   - whatever starts next (`hydra up` in any checkout, `hydra update`) goes
//     through the slots of removed worktrees, finishes the job and frees
//     them (reapGone).
//
// A slot is only freed after what held its ports is gone.

// reapGone cleans up after every removed worktree that still holds a slot,
// and says what it did. A failure leaves that slot taken and is tried again
// by the next call.
func reapGone(co gitx.Checkout, out io.Writer) {
	gone, _ := slots.Gone(co.CommonDir)
	for _, e := range gone {
		did, err := reap(co, e)
		name := filepath.Base(e.Root)
		if err != nil {
			fmt.Fprintf(out, "hydra: warning: %s was removed, and cleaning up after it failed (slot %d stays taken; the next start tries again): %v\n", name, e.Slot, err)
			continue
		}
		if len(did) > 0 {
			fmt.Fprintf(out, "hydra: %s was removed: %s; slot %d is free again\n", name, strings.Join(did, ", "), e.Slot)
		}
	}
}

// reap removes what a worktree's slot holds and then frees the slot: its
// stack if it still runs, the Supabase stacks made for it (destroyed when
// the worktree is gone, only stopped when it is still there), its routes and
// its socket. It returns what it did, for a message.
func reap(co gitx.Checkout, e slots.Entry) ([]string, error) {
	did, err := stopLeftBehind(mainStackOf(co), e.Root, e.Missing())
	if err != nil {
		return did, err
	}
	if c := pc.Unix(socketFor(e.Root)); c.Alive() {
		return did, fmt.Errorf("its process-compose still answers on %s", c.Where)
	}
	routes.Remove(co.CommonDir, e.Slot)
	os.Remove(socketFor(e.Root))
	return did, slots.Release(co.CommonDir, e.Root)
}

// mainStackOf loads the main checkout's stack file, which says what kinds of
// service a worktree may have had. nil when it can't be read: then only the
// stack itself is stopped.
func mainStackOf(co gitx.Checkout) *config.Stack {
	st, err := config.Load(filepath.Join(co.Main, "process-compose.yaml"))
	if err != nil {
		return nil
	}
	return st
}

// stopLeftBehind stops what runs for the worktree at root: the Supabase
// stacks made for it, then its process-compose project. With gone, the
// worktree's directory no longer exists and nobody can come back to its
// database, so the Supabase stacks are destroyed rather than stopped.
func stopLeftBehind(st *config.Stack, root string, gone bool) ([]string, error) {
	var did []string
	if st != nil && hasKind(st, config.KindSupabase) {
		n, err := retireSupabase(root, "", gone)
		if err != nil {
			return did, fmt.Errorf("its Supabase stack: %w", err)
		}
		if n > 0 && gone {
			did = append(did, "removed its Supabase stack")
		} else if n > 0 {
			did = append(did, "stopped its Supabase stack")
		}
	}
	sock := socketFor(root)
	if c := pc.Unix(sock); c.Alive() {
		if err := downSocket(sock, 2*time.Minute); err != nil {
			return did, fmt.Errorf("stopping its stack: %w", err)
		}
		did = append(did, "stopped its stack")
	}
	return did, nil
}

func hasKind(st *config.Stack, kind string) bool {
	for _, s := range st.Services {
		if s.Kind == kind {
			return true
		}
	}
	return false
}

// downSocket stops the process-compose project behind a worktree's socket
// and waits until it is gone.
func downSocket(sock string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "process-compose", "down", "-U", "-u", sock)
	cmd.Env = withoutAgentMarks(os.Environ())
	out, err := cmd.CombinedOutput()
	c := pc.Unix(sock)
	for i := 0; i < 50 && c.Alive(); i++ {
		time.Sleep(200 * time.Millisecond)
	}
	if c.Alive() {
		if err != nil {
			return fmt.Errorf("process-compose down: %v: %s", err, lastLine(out))
		}
		return fmt.Errorf("process-compose still answers on %s after `down`", sock)
	}
	return nil
}

func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// stopGone is the watcher's part: its worktree's directory is gone, so it
// stops the stack it belongs to. The slot is left for reapGone, which frees
// it once nothing answers here any more.
func stopGone(t *target) {
	logf("%s is gone (the worktree was removed): stopping its stack", t.checkout.Root)
	if t.stack != nil && hasKind(t.stack, config.KindSupabase) {
		if n, err := retireSupabase(t.checkout.Root, "", true); err != nil {
			logf("removing its Supabase stack: %v", err)
		} else if n > 0 {
			logf("removed its Supabase stack")
		}
	}
	// The watcher is one of the processes this stops, so it is the last thing
	// it does.
	cmd := exec.Command("process-compose", "down", "-U", "-u", socketFor(t.checkout.Root))
	cmd.Env = withoutAgentMarks(os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		logf("process-compose down: %v: %s", err, lastLine(out))
	}
}

// supabaseStack is one entry of `supabase stack list`: a stack the Supabase
// CLI keeps for a directory and the branch that was checked out there.
type supabaseStack struct {
	ID     string `json:"id"`
	Root   string `json:"project_root"`
	Branch string `json:"branch_context"` // "refs/heads/NAME", or "detached"
	Owner  string `json:"owner"`          // "reachable" while it runs
}

// supabaseCLI runs the Supabase CLI in stack mode, from a directory that
// always exists: the worktree it is asked about may be gone.
func supabaseCLI(args ...string) ([]byte, error) {
	cmd := exec.Command("supabase", args...)
	if cmd.Dir, _ = os.UserHomeDir(); cmd.Dir == "" {
		cmd.Dir = os.TempDir()
	}
	cmd.Env = append(withoutAgentMarks(os.Environ()), "SUPABASE_EXPERIMENTAL_STACK=1")
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		return out, fmt.Errorf("supabase %s: %s", strings.Join(args, " "), lastLine(append(out, ee.Stderr...)))
	}
	return out, err
}

// supabaseStacks lists the stacks the CLI keeps for the worktree at root,
// whatever branch each was made on. The CLI names a stack after the
// directory and the branch together, so a worktree whose branch was renamed
// has a stack its own commands no longer find; hydra goes by the directory.
func supabaseStacks(run func(...string) ([]byte, error), root string) ([]supabaseStack, error) {
	out, err := run("stack", "list", "--output-format", "json")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Stacks []supabaseStack `json:"stacks"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("reading `supabase stack list`: %w", err)
	}
	real := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		real = r
	}
	var mine []supabaseStack
	for _, s := range doc.Stacks {
		if s.Root == root || s.Root == real {
			mine = append(mine, s)
		}
	}
	return mine, nil
}

// retireSupabase stops the Supabase stacks of the worktree at root, or
// destroys them (containers and data) with destroy. keep names the branch
// whose stack is left alone ("refs/heads/NAME"); "" spares none. It returns
// how many it retired. Without the Supabase CLI there is nothing to retire.
func retireSupabase(root, keep string, destroy bool) (int, error) {
	if _, err := exec.LookPath("supabase"); err != nil {
		return 0, nil
	}
	return retireSupabaseWith(supabaseCLI, root, keep, destroy)
}

func retireSupabaseWith(run func(...string) ([]byte, error), root, keep string, destroy bool) (int, error) {
	stacks, err := supabaseStacks(run, root)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, s := range stacks {
		if keep != "" && s.Branch == keep {
			continue
		}
		switch {
		case destroy:
			_, err = run("stack", "destroy", "--stack-id", s.ID, "--yes")
		case s.Owner == "reachable":
			_, err = run("stop", "--stack-id", s.ID)
		default:
			continue // already stopped
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}
