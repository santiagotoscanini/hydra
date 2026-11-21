package stack

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo builds main <- b1 <- b2 <- b3, plus c forked from b1, each in its own
// worktree. b3 has no commits of its own yet, only an uncommitted edit.
func repo(t *testing.T) (dir string, wt func(string) string) {
	t.Helper()
	dir = t.TempDir()
	run := func(d string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = d
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	commit := func(d, file string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(d, file)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, file), []byte(file), 0o644); err != nil {
			t.Fatal(err)
		}
		run(d, "add", ".")
		run(d, "commit", "-qm", file)
	}
	main := filepath.Join(dir, "main")
	os.Mkdir(main, 0o755)
	run(main, "init", "-q", "-b", "main")
	commit(main, "README")

	wt = func(name string) string { return filepath.Join(dir, name) }
	run(main, "worktree", "add", "-q", "-b", "b1", wt("b1"), "main")
	commit(wt("b1"), "db/001.sql")
	run(main, "worktree", "add", "-q", "-b", "b2", wt("b2"), "b1")
	commit(wt("b2"), "api/main.go")
	run(main, "worktree", "add", "-q", "-b", "b3", wt("b3"), "b2")
	os.MkdirAll(filepath.Join(wt("b3"), "web"), 0o755)
	os.WriteFile(filepath.Join(wt("b3"), "web", "page.tsx"), []byte("x"), 0o644)
	run(main, "worktree", "add", "-q", "-b", "c", wt("c"), "b1")
	commit(wt("c"), "worker/job.go")
	// main moves on after the stack was cut.
	commit(main, "CHANGELOG")
	return dir, wt
}

func names(ls []Layer) string {
	var out []string
	for _, l := range ls {
		out = append(out, l.Name+"<"+l.Parent)
	}
	return strings.Join(out, " ")
}

func TestChain(t *testing.T) {
	_, wt := repo(t)
	cases := map[string]string{
		"b1": "b1<main",
		"b2": "b1<main b2<b1",
		"b3": "b1<main b2<b1 b3<b2", // no commits yet: the reflog settles it
		"c":  "b1<main c<b1",        // a sibling of b2, not stacked on it
	}
	for w, want := range cases {
		chain, _, err := Chain(wt(w), Options{BaseRef: "main"})
		if err != nil {
			t.Fatalf("%s: %v", w, err)
		}
		if got := names(chain); got != want {
			t.Errorf("Chain(%s) = %s, want %s", w, got, want)
		}
	}
}

func TestParentOverride(t *testing.T) {
	_, wt := repo(t)
	chain, _, err := Chain(wt("b3"), Options{BaseRef: "main", ParentOverride: func(root string) string {
		if filepath.Base(root) == "b3" {
			return "main"
		}
		return ""
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(chain); got != "b3<main" {
		t.Errorf("got %s, want b3<main", got)
	}
}

// Siblings cut from a commit that no worktree has checked out (like three
// branches off one shared migration commit) aren't stacked on each other.
func TestSiblingsOfAnUncheckedOutCommit(t *testing.T) {
	_, wt := repo(t)
	run := func(d string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = d
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	shared := strings.TrimSpace(func() string {
		out, _ := exec.Command("git", "-C", wt("b1"), "rev-parse", "HEAD").Output()
		return string(out)
	}())
	for _, name := range []string{"s1", "s2"} {
		run(wt("b1"), "worktree", "add", "-q", "-b", name, wt(name), shared)
		os.WriteFile(filepath.Join(wt(name), name), []byte(name), 0o644)
		run(wt(name), "add", ".")
		run(wt(name), "commit", "-qm", name)
	}
	// Take b1, b2 and c out of the picture: only the siblings remain.
	for _, name := range []string{"b3", "b2", "c", "b1"} {
		run(wt("s1"), "worktree", "remove", "--force", wt(name))
	}
	chain, warnings, err := Chain(wt("s1"), Options{BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(chain); got != "s1<main" {
		t.Errorf("got %s, want s1<main", got)
	}
	if len(warnings) != 0 {
		t.Errorf("a sibling is simply not a parent: %v", warnings)
	}
}

// b1 keeps moving after b2 was cut from it: the two diverge, and b2 is still
// stacked on b1 because its reflog says it was created from b1.
func TestParentThatMovedOn(t *testing.T) {
	_, wt := repo(t)
	cmd := exec.Command("git", "commit", "-q", "--allow-empty", "-m", "more db work")
	cmd.Dir = wt("b1")
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	chain, _, err := Chain(wt("b2"), Options{BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(chain); got != "b1<main b2<b1" {
		t.Errorf("got %s, want b1<main b2<b1", got)
	}
}

// The local base branch is behind its remote: a worktree cut from origin/main
// forked there, and what was merged in between isn't its work.
func TestBaseBranchBehindItsRemote(t *testing.T) {
	_, wt := repo(t)
	run := func(d string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = d
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	main := wt("main")
	local := run(main, "rev-parse", "HEAD")
	// Someone else's merged commit, fetched but not pulled.
	run(main, "commit", "-q", "--allow-empty", "-m", "merged upstream")
	remote := run(main, "rev-parse", "HEAD")
	run(main, "reset", "-q", "--hard", local)
	run(main, "remote", "add", "origin", "/nonexistent")
	run(main, "update-ref", "refs/remotes/origin/main", remote)
	run(main, "config", "branch.main.remote", "origin")
	run(main, "config", "branch.main.merge", "refs/heads/main")

	run(main, "worktree", "add", "-q", "-b", "d", wt("d"), "origin/main")
	run(wt("d"), "commit", "-q", "--allow-empty", "-m", "d's work")

	chain, _, err := Chain(wt("d"), Options{BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(chain); got != "d<main" {
		t.Fatalf("got %s, want d<main", got)
	}
	if chain[0].Base != remote {
		t.Errorf("base = %s, want origin/main %s (local main is %s)", chain[0].Base, remote, local)
	}

	// A stack keeps working when its base is the remote's commit.
	run(main, "worktree", "add", "-q", "-b", "e", wt("e"), "d")
	chain, _, err = Chain(wt("e"), Options{BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(chain); got != "d<main e<d" {
		t.Errorf("got %s, want d<main e<d", got)
	}
}
