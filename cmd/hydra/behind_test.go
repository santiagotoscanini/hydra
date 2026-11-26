package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFarBehind(t *testing.T) {
	co := gitRepo(t)
	dir := co.Root
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(file, content string) string {
		t.Helper()
		os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644)
		git("add", "-A")
		git("commit", "-q", "-m", file)
		return git("rev-parse", "HEAD")
	}
	commit("a.go", "one\n")
	fork := git("rev-parse", "HEAD")
	// Main moves on: three commits, one of them to a.go.
	commit("b.go", "b\n")
	commit("c.go", "c\n")
	mainHead := commit("a.go", "main's\n")
	// A branch from before that, with a commit of its own that doesn't meet main's.
	git("checkout", "-q", "-b", "clean", fork)
	clean := commit("d.go", "d\n")
	// And one that edits a.go too.
	git("checkout", "-q", "-b", "clash", fork)
	clash := commit("a.go", "the branch's\n")

	if f := farBehindOf(dir, clean, mainHead, 3, 100); f != nil {
		t.Errorf("3 behind, merging cleanly: %+v, want nothing", f)
	}
	f := farBehindOf(dir, clean, mainHead, 3, 2)
	if f == nil || f.Behind != 3 || len(f.Conflicts) != 0 {
		t.Fatalf("3 behind, past behind_warn 2: %+v", f)
	}
	if p := f.problem("main"); p.Blocks || p.Text != "3 commits behind main (more than behind_warn, 2): the longer it waits, the harder the merge." || p.Fix != "hydra update --here" {
		t.Errorf("problem = %+v", p)
	}
	f = farBehindOf(dir, clash, mainHead, 3, 100)
	if f == nil || strings.Join(f.Conflicts, ",") != "a.go" {
		t.Fatalf("a conflict: %+v", f)
	}
	if p := f.problem("main"); p.Blocks || p.Text != "3 commits behind main; merging it conflicts in 1 file: a.go" || !strings.HasPrefix(p.Fix, "merge main here and resolve them: git merge main") {
		t.Errorf("problem = %+v", p)
	}
	if farBehindOf(dir, clash, mainHead, 0, 100) != nil {
		t.Error("not behind at all, yet far behind")
	}
	// The answer for a pair of commits is kept: a later look doesn't merge again.
	mergeTrees.Lock()
	mergeTrees.m[clean+" "+mainHead] = []string{"cached.go"}
	mergeTrees.Unlock()
	if f := farBehindOf(dir, clean, mainHead, 3, 100); f == nil || f.Conflicts[0] != "cached.go" {
		t.Errorf("not from the cache: %+v", f)
	}
	// A commit git doesn't know: nothing to say, rather than a guess.
	if f := farBehindOf(dir, "0123456789012345678901234567890123456789", mainHead, 300, 100); f != nil {
		t.Errorf("an unknown commit: %+v", f)
	}
}
