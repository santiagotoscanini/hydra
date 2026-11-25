package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIdleClock(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	c := newIdleClock(t0)

	// What `git status` says on the first look is where it starts, not use.
	c.see(at(time.Minute), " M a.go")
	if got := c.idle(at(time.Minute)); got != time.Minute {
		t.Fatalf("idle after the first look = %v, want 1m", got)
	}
	// The same status, and stamps older than the start: still idle.
	c.see(at(30*time.Minute), " M a.go", at(-time.Hour), time.Time{})
	if got := c.idle(at(30 * time.Minute)); got != 30*time.Minute {
		t.Errorf("idle = %v, want 30m", got)
	}
	// An edit that changes what git lists counts from the moment it is seen.
	c.see(at(40*time.Minute), " M a.go\x00?? b.go")
	if got := c.idle(at(50 * time.Minute)); got != 10*time.Minute {
		t.Errorf("idle after a new file = %v, want 10m", got)
	}
	// An edit to a file git already lists: its modification time.
	c.see(at(60*time.Minute), " M a.go\x00?? b.go", at(55*time.Minute))
	if got := c.idle(at(60 * time.Minute)); got != 5*time.Minute {
		t.Errorf("idle after an edit = %v, want 5m", got)
	}
	// A request through the router, noted in the future by a clock that
	// moved: now.
	c.see(at(70*time.Minute), " M a.go\x00?? b.go", at(3*time.Hour))
	if got := c.idle(at(70 * time.Minute)); got != 0 {
		t.Errorf("idle after a stamp from the future = %v, want 0", got)
	}
	// Committing everything empties git status: that is use too.
	c.see(at(3*time.Hour), "")
	if got := c.idle(at(3 * time.Hour)); got != 0 {
		t.Errorf("idle after a commit = %v, want 0", got)
	}

	// A clean worktree from the start: the first edit counts.
	c = newIdleClock(t0)
	c.see(at(time.Minute), "")
	c.see(at(2*time.Hour), " M a.go")
	if got := c.idle(at(2 * time.Hour)); got != 0 {
		t.Errorf("the first edit in a clean worktree: idle = %v, want 0", got)
	}
}

func TestUsedAt(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-3 * time.Hour)
	newer := time.Now().Add(-time.Hour)
	write := func(rel string, mod time.Time) {
		path := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte(rel), 0o644)
		os.Chtimes(path, mod, mod)
	}
	write("a.go", old)
	write("dir/b.go", newer)
	if got := usedAt(root, []byte(" M a.go\x00")); !got.Equal(old) {
		t.Errorf("one file: %v, want %v", got, old)
	}
	// The newest of what git lists; a deleted one is skipped.
	if got := usedAt(root, []byte(" M a.go\x00?? dir/b.go\x00 D gone.go\x00")); !got.Equal(newer) {
		t.Errorf("two files: %v, want %v", got, newer)
	}
	// The router's note.
	noteActivity(root, time.Now()) // no .process-compose: nothing is written
	if _, err := os.Stat(activityFile(root)); err == nil {
		t.Fatal("noteActivity made a .process-compose folder")
	}
	os.Mkdir(filepath.Join(root, ".process-compose"), 0o755)
	activityLast = map[string]time.Time{}
	now := time.Now().Truncate(time.Second)
	noteActivity(root, now)
	if got := usedAt(root, []byte(" M a.go\x00")); !got.Equal(now) {
		t.Errorf("after a request: %v, want %v", got, now)
	}
	// Requests in a burst write once.
	noteActivity(root, now.Add(activityEvery/2))
	if got := usedAt(root, nil); !got.Equal(now) {
		t.Errorf("a second request soon after wrote again: %v", got)
	}
	noteActivity(root, now.Add(activityEvery))
	if got := usedAt(root, nil); !got.Equal(now.Add(activityEvery)) {
		t.Errorf("a request %s later: %v", activityEvery, got)
	}
}

func TestIdleNote(t *testing.T) {
	root := t.TempDir()
	if readIdleNote(root) != nil {
		t.Fatal("a note where none was written")
	}
	os.MkdirAll(filepath.Join(root, ".process-compose"), 0o755)
	os.WriteFile(idleNoteFile(root), []byte(`{"at":"2026-11-25T14:02:00Z","after":"2h"}`), 0o644)
	n := readIdleNote(root)
	if n == nil {
		t.Fatal("no note")
	}
	if got := n.text(); !strings.HasPrefix(got, "this worktree's stack was stopped after 2h idle (at Nov 25 ") || !strings.HasSuffix(got, "); hydra up starts it again.") {
		t.Errorf("text = %q", got)
	}
}

func TestFmtIdle(t *testing.T) {
	for d, want := range map[time.Duration]string{
		2 * time.Hour:           "2h",
		90 * time.Minute:        "1h30m",
		45 * time.Second:        "45s",
		5 * time.Minute:         "5m",
		time.Hour + time.Second: "1h0m1s",
	} {
		if got := fmtIdle(d); got != want {
			t.Errorf("fmtIdle(%v) = %q, want %q", d, got, want)
		}
	}
}
