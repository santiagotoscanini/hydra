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
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
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
	os.Mkdir(c, 0o755)
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
