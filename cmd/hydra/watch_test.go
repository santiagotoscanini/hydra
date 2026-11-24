package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The watcher plans only when watchKey changes (and every fullEvery), so
// the key has to change with everything a plan is made from, and stay put
// otherwise.
func TestTheWatchKeyChangesWithWhatAPlanReads(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	co := gitRepo(t)
	file := filepath.Join(co.Main, "process-compose.yaml")
	if err := os.WriteFile(file, []byte(checkStack), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "wt")
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(co.Main, "worktree", "add", "-q", "-b", "feature", root)
	tgt, err := resolve(root, file)
	if err != nil {
		t.Fatal(err)
	}

	key := watchKey(tgt)
	if key == "" {
		t.Fatal("no key in a plain worktree")
	}
	if again := watchKey(tgt); again != key {
		t.Fatal("the key changed with nothing changed")
	}
	changes := []struct {
		what string
		do   func()
	}{
		{"a new file", func() { os.WriteFile(filepath.Join(root, "new.go"), []byte("package x\n"), 0o644) }},
		{"a commit here", func() { git(root, "add", "-A"); git(root, "-c", "commit.gpgsign=false", "commit", "-qm", "x") }},
		{"a commit on main", func() { git(co.Main, "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "y") }},
		{"another worktree", func() { git(co.Main, "worktree", "add", "-q", "-b", "other", filepath.Join(t.TempDir(), "other")) }},
		{"the stack file", func() { os.WriteFile(file, []byte(checkStack+"\n# edited\n"), 0o644) }},
		{".hydra.local.yaml", func() { os.WriteFile(filepath.Join(root, ".hydra.local.yaml"), []byte("with: [api]\n"), 0o644) }},
	}
	for _, c := range changes {
		c.do()
		next := watchKey(tgt)
		if next == key {
			t.Errorf("the key didn't change with %s", c.what)
		}
		key = next
	}
}
