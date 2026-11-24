package slots

import (
	"os"
	"path/filepath"
	"testing"
)

func TestASlotStaysTakenUntilItIsReleased(t *testing.T) {
	common := t.TempDir()
	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	for _, d := range []string{a, b} {
		worktree(t, d)
	}
	slotA, err := Acquire(common, a, 2)
	if err != nil || slotA != 1 {
		t.Fatalf("a: slot %d, %v", slotA, err)
	}
	if again, _ := Acquire(common, a, 2); again != slotA {
		t.Errorf("a worktree keeps its slot: got %d, then %d", slotA, again)
	}

	// The worktree is removed. Whatever it ran may still hold the slot's
	// ports, so the slot isn't handed on: b gets the next one.
	if err := os.RemoveAll(a); err != nil {
		t.Fatal(err)
	}
	if slotB, err := Acquire(common, b, 2); err != nil || slotB != 2 {
		t.Fatalf("b: slot %d, %v (a's slot must stay taken)", slotB, err)
	}
	gone, err := Gone(common)
	if err != nil || len(gone) != 1 || gone[0].Root != a || gone[0].Slot != 1 {
		t.Fatalf("Gone = %v, %v", gone, err)
	}
	c := filepath.Join(t.TempDir(), "c")
	worktree(t, c)
	if _, err := Acquire(common, c, 2); err == nil {
		t.Error("a third worktree got a slot while both were taken")
	}

	// Once the caller has cleaned up and released it, the slot is free.
	if err := Release(common, a); err != nil {
		t.Fatal(err)
	}
	if slotC, err := Acquire(common, c, 2); err != nil || slotC != 1 {
		t.Errorf("c: slot %d, %v", slotC, err)
	}
	if gone, _ := Gone(common); len(gone) != 0 {
		t.Errorf("Gone after the release = %v", gone)
	}
}

// worktree makes dir look like a git worktree: a `.git` file naming a git
// directory that exists. It returns that directory.
func worktree(t *testing.T, dir string) string {
	t.Helper()
	admin := t.TempDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+admin+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return admin
}

// git forgets a worktree even when it can't delete all of its folder, which
// is what happens when a stack still writes there. The folder that is left
// says so in one of two ways.
func TestAFolderThatIsNoLongerAWorktreeCountsAsRemoved(t *testing.T) {
	live := filepath.Join(t.TempDir(), "live")
	worktree(t, live)
	if Removed(live) {
		t.Error("a worktree with its `.git` counts as removed")
	}

	// A checkout of its own (`.git` is a directory) is never one.
	clone := t.TempDir()
	os.Mkdir(filepath.Join(clone, ".git"), 0o755)
	if Removed(clone) {
		t.Error("a checkout with a `.git` directory counts as removed")
	}

	if !Removed(filepath.Join(t.TempDir(), "never-there")) {
		t.Error("a folder that doesn't exist isn't removed")
	}

	// git deleted everything, `.git` too, and the stack wrote a log after.
	emptied := filepath.Join(t.TempDir(), "emptied")
	os.MkdirAll(filepath.Join(emptied, ".process-compose"), 0o755)
	if !Removed(emptied) {
		t.Error("a folder without `.git` isn't removed")
	}

	// git gave up before it reached `.git`, and forgot the worktree anyway.
	half := filepath.Join(t.TempDir(), "half")
	if err := os.RemoveAll(worktree(t, half)); err != nil {
		t.Fatal(err)
	}
	if !Removed(half) {
		t.Error("a folder whose `.git` names a git directory that is gone isn't removed")
	}

	// A path relative to the worktree, as worktree.useRelativePaths writes.
	rel := filepath.Join(t.TempDir(), "rel")
	os.MkdirAll(filepath.Join(rel, "admin"), 0o755)
	os.WriteFile(filepath.Join(rel, ".git"), []byte("gitdir: admin\n"), 0o644)
	if Removed(rel) {
		t.Error("a relative gitdir that exists counts as removed")
	}
	os.Remove(filepath.Join(rel, "admin"))
	if !Removed(rel) {
		t.Error("a relative gitdir that is gone isn't removed")
	}

	// Something else in `.git`: not for this to judge.
	odd := filepath.Join(t.TempDir(), "odd")
	os.MkdirAll(odd, 0o755)
	os.WriteFile(filepath.Join(odd, ".git"), []byte("not a gitfile\n"), 0o644)
	if Removed(odd) {
		t.Error("an unreadable `.git` counts as removed")
	}
}

func TestALookCreatesNothing(t *testing.T) {
	common := t.TempDir()
	if entries, err := List(common); err != nil || len(entries) != 0 {
		t.Fatalf("List = %v, %v", entries, err)
	}
	if gone, err := Gone(common); err != nil || len(gone) != 0 {
		t.Fatalf("Gone = %v, %v", gone, err)
	}
	if _, err := os.Stat(filepath.Join(common, fileName)); !os.IsNotExist(err) {
		t.Errorf("looking at the slots created the registry (stat: %v)", err)
	}
	// Without a repository there is nothing to list, and nothing is written
	// into the current directory.
	if entries, err := List(""); err != nil || len(entries) != 0 {
		t.Errorf("List(\"\") = %v, %v", entries, err)
	}
	if _, err := os.Stat(fileName); !os.IsNotExist(err) {
		t.Errorf("List(\"\") left %s in the current directory", fileName)
	}
}
