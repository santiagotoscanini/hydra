package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/journal"
	"github.com/santiagotoscanini/hydra/internal/pc"
)

// A worktree's stack holds a gigabyte or two for as long as it runs, and an
// agent that is done with a worktree leaves it running. With x-hydra
// idle_stop, the worktree's watcher stops the stack once nobody has used it
// for that long, the way `hydra down` there would. Using it means one of:
//
//   - a request for the worktree through hydra's router (its port for a
//     routed service, or a call tagged with it), which the router, shared by
//     every worktree, notes in the worktree's activityFile;
//   - a change to its files: what `git status` lists, or the modification
//     time of a file it lists.
//
// Requests that go straight to a service the worktree runs itself don't
// pass the router, so they aren't seen; the edits that go with them are.
// The main checkout has no watcher, and never stops.

// activityFile is touched by the router when a request for the worktree at
// root goes through it. Its modification time is what counts.
func activityFile(root string) string {
	return filepath.Join(root, ".process-compose", "hydra-activity")
}

// idleNoteFile says that the stack was stopped for being idle, until it
// starts again.
func idleNoteFile(root string) string {
	return filepath.Join(root, ".process-compose", "idle-stopped")
}

// activityEvery is how often the router writes a worktree's activityFile at
// most: a page load makes dozens of requests.
const activityEvery = time.Second

var (
	activityMu   sync.Mutex
	activityLast = map[string]time.Time{}
)

// noteActivity records a request for the worktree at root. Only into a
// .process-compose folder that is there: a worktree that is being removed
// gets nothing written into it.
func noteActivity(root string, now time.Time) {
	if root == "" {
		return
	}
	activityMu.Lock()
	due := now.Sub(activityLast[root]) >= activityEvery
	if due {
		activityLast[root] = now
	}
	activityMu.Unlock()
	if !due {
		return
	}
	path := activityFile(root)
	if err := os.Chtimes(path, now, now); errors.Is(err, os.ErrNotExist) {
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			f.Close()
			os.Chtimes(path, now, now)
		}
	}
}

// idleClock is when the worktree was last used, as the watcher sees it.
type idleClock struct {
	last   time.Time
	status string // what `git status` said last
	seen   bool   // status was set
}

func newIdleClock(now time.Time) *idleClock { return &idleClock{last: now} }

// see is one look: status is `git status` now, and stamps are times
// something was last used (the router's note, the newest edit). A status
// that differs from the last one counts as use now; a stamp from the future
// (a clock that moved) counts as now.
func (c *idleClock) see(now time.Time, status string, stamps ...time.Time) {
	if c.seen && status != c.status {
		c.last = now
	}
	c.status, c.seen = status, true
	for _, s := range stamps {
		if s.After(now) {
			s = now
		}
		if s.After(c.last) {
			c.last = s
		}
	}
}

// idle is how long the worktree has gone unused.
func (c *idleClock) idle(now time.Time) time.Duration { return now.Sub(c.last) }

// usedAt is when the worktree at root was last used, going by the router's
// note and by the newest of the files `git status` lists (status is its
// -z output).
func usedAt(root string, status []byte) time.Time {
	var newest time.Time
	if fi, err := os.Stat(activityFile(root)); err == nil {
		newest = fi.ModTime()
	}
	const most = 5000 // a folder of generated files nobody ignored
	for i, entry := range bytes.Split(status, []byte{0}) {
		if i == most {
			break
		}
		if len(entry) < 4 {
			continue
		}
		if fi, err := os.Lstat(filepath.Join(root, string(entry[3:]))); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	return newest
}

// idleNote is what idleNoteFile holds.
type idleNote struct {
	At    time.Time `json:"at"`
	After string    `json:"after"` // the idle_stop it was stopped by: "2h"
}

// readIdleNote returns the note of an idle stop of the stack at root, nil
// when there is none.
func readIdleNote(root string) *idleNote {
	data, err := os.ReadFile(idleNoteFile(root))
	if err != nil {
		return nil
	}
	var n idleNote
	if json.Unmarshal(data, &n) != nil {
		return nil
	}
	return &n
}

// text is how `hydra status` says it.
func (n *idleNote) text() string {
	return fmt.Sprintf("this worktree's stack was stopped after %s idle (at %s); hydra up starts it again.", n.After, n.At.Local().Format("Jan 2 15:04"))
}

// fmtIdle writes a duration the way it is configured: "2h", "1h30m", "45s".
func fmtIdle(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// stopIdle is the watcher's part: nobody used the worktree for after. The
// watcher is one of the processes the stop ends, so, as for a removed
// worktree (stopGone), it starts `hydra stop-idle` in a session of its own
// to see the stop through.
func stopIdle(t *target, after time.Duration) {
	root := t.checkout.Root
	logf("nothing used this worktree for %s (idle_stop): stopping its stack; hydra up starts it again", fmtIdle(after))
	exe, err := stableExecutable()
	if err == nil {
		cmd := exec.Command(exe, "stop-idle", root, fmtIdle(after))
		cmd.Dir = root
		cmd.Env = withoutAgentMarks(os.Environ())
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err = cmd.Start(); err == nil {
			cmd.Process.Release()
			return
		}
	}
	logf("can't start hydra stop-idle (%v); the stack keeps running", err)
}

// stopIdleCmd is `hydra stop-idle ROOT AFTER`, which a worktree's watcher
// starts once nobody used the worktree for AFTER: it stops the stack the way
// `hydra down` does, and leaves the note `hydra status` shows.
func stopIdleCmd(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: hydra stop-idle ROOT AFTER (started by a worktree's watcher; by hand: hydra down)")
	}
	root, after := args[0], args[1]
	co, err := gitx.Find(root)
	if err != nil {
		return err
	}
	if co.IsMain() {
		return fmt.Errorf("%s is the main checkout, whose stack never stops for being idle", root)
	}
	sock := socketPath(co)
	if !pc.Unix(sock).Alive() {
		return nil // stopped already
	}
	if _, err := downSocket(sock, 3*time.Minute); err != nil {
		journal.Log("idle", journal.Fields{"after": after, "error": err.Error()})
		return fmt.Errorf("stopping the idle stack of %s: %w", root, err)
	}
	note, _ := json.Marshal(idleNote{At: time.Now(), After: after})
	if err := writeFile(idleNoteFile(co.Root), note); err != nil {
		return err
	}
	journal.Log("idle", journal.Fields{"after": after})
	return nil
}
