package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/pc"
	"github.com/santiagotoscanini/hydra/internal/routes"
	"github.com/santiagotoscanini/hydra/internal/slots"
)

// A worktree that is removed while its stack runs leaves the stack behind:
// process-compose and the services keep running with no checkout under
// them, on ports the slot would hand to the next worktree, and its own
// Supabase stack keeps its containers and data. Usually a folder stays too.
// The stack writes into it (logs, caches, build output) while git deletes
// it, and git gives up at the first directory that isn't empty once it has
// emptied it. What is left is a folder holding only those files, or most of
// the checkout with a `.git` that leads nowhere; git forgets the worktree
// either way (slots.Removed tells both).
//
// Nothing tells hydra when a worktree is removed (git has no hook for it),
// so hydra cleans up when it next looks:
//
//   - the worktree's own watcher notices at once and starts `hydra reap`,
//     a process of its own, since the watcher is part of the stack that has
//     to stop (stopGone);
//   - whatever starts next (`hydra up` in any checkout, `hydra update`) goes
//     through the slots of removed worktrees and does the same for what is
//     left (reapGone).
//
// Both end in reap. A slot is only freed after what held its ports is gone.

// presence is what the folder a slot was given to is to git.
type presence int

const (
	present presence = iota // a worktree
	leaving                 // not one any more, though git still lists it
	removed                 // git has forgotten it
)

// presenceOf asks git. `leaving` is a worktree git is in the middle of
// removing (it deletes the files first and forgets the worktree last), or
// one whose `.git` was deleted by hand. Only time tells them apart, and only
// the first is safe to clean up after: the second can be mended
// (`git worktree repair`), and then wants its database back.
func presenceOf(main, root string) presence {
	switch {
	case !slots.Removed(root):
		return present
	case listedWorktree(main, root):
		return leaving
	}
	return removed
}

// listedWorktree reports whether git still has a worktree at root. When git
// can't be asked, it says yes: nothing is destroyed on a guess.
func listedWorktree(main, root string) bool {
	out, err := gitOut(main, "worktree", "list", "--porcelain")
	if err != nil {
		return true
	}
	real := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		real = r
	}
	for _, line := range strings.Split(out, "\n") {
		if path, ok := strings.CutPrefix(line, "worktree "); ok && (path == root || path == real) {
			return true
		}
	}
	return false
}

// reaping is how far reap goes for a worktree that is no longer one.
type reaping struct {
	// patience is how long git gets to finish removing a worktree it still
	// lists.
	patience time.Duration
	// anyway frees the slot of a worktree git still lists after that, the
	// way a present one's is freed: its database is stopped and kept
	// (`hydra slots free`). Without it the slot waits for a later look.
	anyway bool
	// saw is the worktree's own watcher saying that the worktree was removed
	// under its running stack. What is left in the folder is then what git
	// gave up on, and goes too.
	saw bool
}

// gitPatience is what the watcher's reap gives git: deleting a large
// checkout takes a while.
var gitPatience = 5 * time.Minute

// reaped is what reap did.
type reaped struct {
	did   []string // for a message
	freed bool     // the slot is free
	left  bool     // the worktree is gone, and its folder still holds files
}

// reapGone cleans up after every removed worktree that still holds a slot,
// and says what it did. A failure leaves that slot taken and is tried again
// by the next call.
func reapGone(co gitx.Checkout, out io.Writer) {
	gone, _ := slots.Gone(co.CommonDir)
	for _, e := range gone {
		r, err := reap(co, e, reaping{})
		name := filepath.Base(e.Root)
		switch {
		case err != nil:
			fmt.Fprintf(out, "hydra: warning: %s was removed, and cleaning up after it failed (slot %d stays taken; the next start tries again): %v\n", name, e.Slot, err)
		case r.freed && len(r.did) > 0:
			fmt.Fprintf(out, "hydra: %s was removed: %s; slot %d is free again\n", name, strings.Join(r.did, ", "), e.Slot)
		case len(r.did) > 0:
			fmt.Fprintf(out, "hydra: %s is no longer a worktree: %s (git still lists it, so it keeps slot %d and its database for now)\n", name, strings.Join(r.did, ", "), e.Slot)
		}
		if r.left {
			fmt.Fprintln(out, leftNote(e.Root))
		}
	}
}

func leftNote(root string) string {
	return "hydra: its folder still holds files git didn't delete: rm -rf " + shQuote(root)
}

// reap removes what a worktree's slot holds and then frees the slot: its
// stack if it still runs, the Supabase stacks made for it, its routes and
// its socket. A worktree that is still there keeps its database, stopped;
// one git has forgotten loses it, and what hydra wrote in its folder.
func reap(co gitx.Checkout, e slots.Entry, how reaping) (reaped, error) {
	var r reaped
	// One at a time: the watcher's reap and a `hydra up` somewhere else, or
	// two of those, may come for the same worktree.
	unlock, mine := lockReap(e.Root, how.anyway || how.saw)
	if !mine {
		return r, nil // another hydra is on it
	}
	freed := false
	defer func() { unlock(freed) }()
	if !holdsSlot(co.CommonDir, e.Root) {
		r.freed = true // that other hydra finished while this one waited
		return r, nil
	}

	// What runs goes first, whatever git ends up saying: without a checkout
	// under it nothing there works, and it keeps writing into the folder.
	// Its stop of the database is by directory, which a folder that is no
	// longer a worktree can't be trusted with: there the rendered stop does
	// nothing (`hydra prepare supabase --stopping`), and the stacks are dealt
	// with below, by id.
	sock := socketFor(e.Root)
	if c := pc.Unix(sock); c.Alive() {
		if _, err := downSocket(sock, 2*time.Minute); err != nil {
			return r, fmt.Errorf("stopping its stack: %w", err)
		}
		r.did = append(r.did, "stopped its stack")
	}

	// A process-compose that was killed there stopped nothing.
	var names []string
	st := mainStackOf(co)
	if st != nil {
		names = serviceNames(st)
	}
	if found := leftBehind(st, e.Root, e.Slot, names, []string{e.Root}); len(found) > 0 {
		if len(stopLeftovers(found)) < len(found) {
			return r, fmt.Errorf("stopping what its stack left running (%s): not all of it stopped", leftoverNames(found, true))
		}
		r.did = append(r.did, "stopped what its stack had left running ("+leftoverNames(found, false)+")")
	}

	state := presenceOf(co.Main, e.Root)
	for until := time.Now().Add(how.patience); state == leaving && time.Now().Before(until); state = presenceOf(co.Main, e.Root) {
		time.Sleep(500 * time.Millisecond)
	}
	if state == leaving && !how.anyway {
		return r, nil
	}

	if st := mainStackOf(co); st != nil && hasKind(st, config.KindSupabase) {
		n, err := retireSupabase(e.Root, "", state == removed)
		switch {
		case n > 0 && state == removed:
			r.did = append(r.did, "removed its Supabase stack")
		case n > 0:
			r.did = append(r.did, "stopped its Supabase stack")
		}
		if err != nil {
			return r, fmt.Errorf("its Supabase stack: %w", err)
		}
	}
	if state == removed {
		cleared, left := clearFolder(co.Main, e.Root, how.saw)
		if cleared {
			r.did = append(r.did, "deleted what was left of its folder")
		}
		r.left = left
	}
	why := "slot freed"
	if state == removed {
		why = "worktree removed"
	}
	for _, name := range releaseTunnelsOf(co.CommonDir, e.Root, why, nil) {
		r.did = append(r.did, "gave tunnel "+name+" back to main")
	}
	routes.Remove(co.CommonDir, e.Slot)
	os.Remove(sock)
	os.Remove(runningFile(e.Root))
	if err := slots.Release(co.CommonDir, e.Root); err != nil {
		return r, err
	}
	freed, r.freed = true, true
	return r, nil
}

// lockReap makes cleaning up after one worktree one process's job. With
// wait it waits its turn; without, it reports false when another process
// has it. The returned function gives the lock back, and with done (the
// slot is freed, so nobody comes for this worktree again) removes it.
func lockReap(root string, wait bool) (unlock func(done bool), mine bool) {
	path := strings.TrimSuffix(socketFor(root), ".sock") + ".reap"
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return func(bool) {}, true // no lock to be had: as careful as before
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return func(bool) {}, false
	}
	return func(done bool) {
		if done {
			os.Remove(path)
		}
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, true
}

func holdsSlot(commonDir, root string) bool {
	for _, e := range mustList(commonDir) {
		if e.Root == root {
			return true
		}
	}
	return false
}

// clearFolder deletes what is left in the folder of a worktree git has
// forgotten, now that its stack has stopped. cleared: there was a folder,
// and it is gone. left: it is still there, with files in it.
//
// With all (the worktree's watcher saw it removed under a running stack),
// everything goes: git was told to delete the folder and gave up because the
// stack kept writing into it. Otherwise only what hydra wrote goes, and the
// folder if that empties it. A folder found later may have another story
// (a volume that was away while git pruned the worktree), and nothing in it
// is hydra's to judge.
func clearFolder(main, root string, all bool) (cleared, left bool) {
	if _, err := os.Lstat(root); err != nil {
		return false, false // git deleted all of it
	}
	if all && !inside(main, root) && os.RemoveAll(root) == nil {
		return true, false
	}
	os.RemoveAll(filepath.Join(root, ".process-compose"))
	env := filepath.Join(root, ".env.local")
	if fi, err := os.Lstat(env); err == nil && fi.Mode().IsRegular() {
		if data, err := os.ReadFile(env); err == nil && strings.Contains(string(data), envBegin) {
			if kept := withoutBlock(string(data)); strings.TrimSpace(kept) == "" {
				os.Remove(env)
			} else {
				os.WriteFile(env, []byte(kept), 0o644)
			}
		}
	}
	// The directory hydra makes for the Supabase CLI's temporary files.
	for _, dir := range []string{"supabase/.temp/tmp", "supabase/.temp", "supabase"} {
		os.Remove(filepath.Join(root, dir)) // only when empty
	}
	if os.Remove(root) == nil {
		return true, false
	}
	return false, true
}

// inside reports whether path is dir or lies in it.
func inside(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// reapCmd is `hydra reap ROOT`, which a worktree's watcher starts when the
// worktree is removed under it (stopGone): it stops the stack, waits for git
// to be done with the folder, and deletes what git left there.
func reapCmd(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: hydra reap ROOT (started by a worktree's watcher when the worktree is removed; by hand: hydra slots free ROOT)")
	}
	co, err := gitx.Find(".")
	if err != nil {
		return err
	}
	for _, e := range mustList(co.CommonDir) {
		if e.Root != args[0] {
			continue
		}
		if !e.Missing() {
			return fmt.Errorf("%s is still a worktree (to stop its stack and free its slot: hydra slots free %s)", e.Root, shQuote(e.Root))
		}
		r, err := reap(co, e, reaping{patience: gitPatience, saw: true})
		if err != nil {
			return fmt.Errorf("slot %d stays taken: %w", e.Slot, err)
		}
		if !r.freed {
			return fmt.Errorf("git still lists %s as a worktree, so it keeps slot %d and its database%s", e.Root, e.Slot, andDid(r.did))
		}
		fmt.Printf("slot %d is free%s\n", e.Slot, andDid(r.did))
		return nil
	}
	return fmt.Errorf("%s holds no slot (`hydra slots` lists them)", args[0])
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

func hasKind(st *config.Stack, kind string) bool {
	for _, s := range st.Services {
		if s.Kind == kind {
			return true
		}
	}
	return false
}

// downSocket stops the process-compose project behind a worktree's socket
// and waits until it is gone. It returns what it had to kill (see
// watchStop).
func downSocket(sock string, timeout time.Duration) ([]killed, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "process-compose", "down", "-U", "-u", sock)
	cmd.Env = withoutAgentMarks(os.Environ())
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	c := pc.Unix(sock)
	kills, err := watchStop(cmd, c, io.Discard)
	for i := 0; i < 50 && c.Alive(); i++ {
		time.Sleep(200 * time.Millisecond)
	}
	if c.Alive() {
		if err != nil {
			return kills, fmt.Errorf("process-compose down: %v: %s", err, lastLine(out.Bytes()))
		}
		return kills, fmt.Errorf("process-compose still answers on %s after `down`", sock)
	}
	return kills, nil
}

func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// stopGone is the watcher's part: its worktree was removed, so the stack it
// belongs to has to stop, its database has to go, and its slot is free for
// the next worktree. The watcher is one of the processes the stop ends, so
// it starts `hydra reap` in a session of its own, from the main checkout,
// and that sees the job through. If it can't be started, the watcher stops
// the stack itself and the next `hydra up` does the rest.
func stopGone(t *target) {
	root := t.checkout.Root
	logf("%s is no longer a worktree (it was removed): stopping its stack", root)
	if exe, err := stableExecutable(); err == nil {
		cmd := exec.Command(exe, "reap", root)
		cmd.Dir = t.checkout.Main
		cmd.Env = withoutAgentMarks(os.Environ())
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err == nil {
			cmd.Process.Release()
			return
		}
	}
	cmd := exec.Command("process-compose", "down", "-U", "-u", socketFor(root))
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
	if root == "" {
		return doc.Stacks, nil // every directory's
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
