package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDefaultBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	want := func(name, why string) {
		t.Helper()
		co, err := Find(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := DefaultBranch(co); got != name {
			t.Errorf("%s: got %q, want %q", why, got, name)
		}
	}

	git("init", "-q", "-b", "trunk")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "f")
	git("commit", "-q", "-m", "init")
	want("trunk", "no remote, no main: the main checkout's branch")

	git("branch", "master")
	want("master", "a local master")
	git("branch", "main")
	want("main", "main before master")

	// What a clone records: origin's default branch.
	git("update-ref", "refs/remotes/origin/develop", "HEAD")
	git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/develop")
	want("origin/develop", "origin's default, not checked out locally")
	git("branch", "develop")
	want("develop", "origin's default, with a local branch")

	// Asked from a worktree, the answer is the same.
	wt := filepath.Join(t.TempDir(), "wt")
	git("worktree", "add", "-q", "-b", "feature", wt)
	co, err := Find(wt)
	if err != nil {
		t.Fatal(err)
	}
	if got := DefaultBranch(co); got != "develop" {
		t.Errorf("from a worktree: got %q, want %q", got, "develop")
	}
}
