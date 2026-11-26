package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santiagotoscanini/hydra/internal/stack"
)

// bareRepo builds main <- schema <- core <- web, only web checked out, and
// main moved on after the stack was cut: schema and core are behind.
// conflict makes main's new commit change schema's file too.
func bareRepo(t *testing.T, conflict bool) (main, web string, g func(dir string, args ...string) string) {
	t.Helper()
	dir := t.TempDir()
	main = filepath.Join(dir, "main")
	os.Mkdir(main, 0o755)
	g = func(d string, args ...string) string {
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
	commit := func(file, text string) {
		os.WriteFile(filepath.Join(main, file), []byte(text), 0o644)
		g(main, "add", ".")
		g(main, "commit", "-qm", file)
	}
	g(main, "init", "-q", "-b", "main")
	g(main, "config", "user.name", "t")
	g(main, "config", "user.email", "t@t")
	commit("README", "x")
	g(main, "checkout", "-q", "-b", "schema")
	commit("schema.sql", "schema")
	g(main, "checkout", "-q", "-b", "core")
	commit("core.go", "core")
	g(main, "checkout", "-q", "-b", "web")
	commit("web.tsx", "web")
	g(main, "checkout", "-q", "main")
	if conflict {
		commit("schema.sql", "main's schema")
	} else {
		commit("CHANGELOG", "main moves on")
	}
	web = filepath.Join(dir, "web")
	g(main, "worktree", "add", "-q", web, "web")
	return main, web, g
}

func bareUpdate(t *testing.T, main, branch string) (*wtUpdate, *stackView) {
	t.Helper()
	wts, err := stack.List(main)
	if err != nil {
		t.Fatal(err)
	}
	view, err := loadStacks(main, "main", wts)
	if err != nil {
		t.Fatal(err)
	}
	if !view.st.IsBare(branch) {
		t.Fatalf("%s isn't a branch with no checkout", branch)
	}
	u := &wtUpdate{name: branch, main: main}
	u.inspect("main", view)
	return u, view
}

func TestBareCleanMerge(t *testing.T) {
	main, _, g := bareRepo(t, false)
	mainHead := g(main, "rev-parse", "main")
	before := g(main, "rev-parse", "schema")

	u, _ := bareUpdate(t, main, "schema")
	if u.parent != "main" || strings.Join(u.chain, " ") != "schema" {
		t.Fatalf("schema: parent %s chain %v", u.parent, u.chain)
	}
	u.decide(main, "main", mainHead, false)
	if u.action != "merge" || u.behind != 1 {
		t.Fatalf("decide: action %q behind %d note %q", u.action, u.behind, u.note)
	}
	u.apply(0)
	if u.hand {
		t.Fatalf("apply: %s", u.note)
	}
	after := g(main, "rev-parse", "schema")
	if parents := g(main, "rev-list", "--parents", "-n1", after); parents != after+" "+before+" "+mainHead {
		t.Errorf("merge commit's parents: %s, want schema's old commit then main", parents)
	}
	if msg := g(main, "log", "-1", "--format=%s", after); msg != "Merge branch 'main' into schema" {
		t.Errorf("message %q", msg)
	}
	if files := g(main, "ls-tree", "--name-only", after); files != "CHANGELOG\nREADME\nschema.sql" {
		t.Errorf("merged tree: %q", files)
	}
	if reflog := g(main, "reflog", "-1", "--format=%gs", "refs/heads/schema"); !strings.Contains(reflog, "hydra update: merge main") {
		t.Errorf("reflog: %q", reflog)
	}
	// main's checkout wasn't touched.
	if st := g(main, "status", "--porcelain"); st != "" {
		t.Errorf("main's checkout changed: %q", st)
	}

	// Next in the chain: core is behind schema now.
	u, _ = bareUpdate(t, main, "core")
	if u.parent != "schema" {
		t.Fatalf("core's parent: %s", u.parent)
	}
	u.decide(main, "main", mainHead, true)
	if u.action != "merge" || !strings.Contains(u.note, "would be merged in") {
		t.Errorf("core: %q %q", u.action, u.note)
	}
}

// The branch moved between decide and apply: update-ref refuses, and the
// branch keeps the other change.
func TestBareMergeGuard(t *testing.T) {
	main, _, g := bareRepo(t, false)
	mainHead := g(main, "rev-parse", "main")
	u, _ := bareUpdate(t, main, "schema")
	u.decide(main, "main", mainHead, false)
	if u.action != "merge" {
		t.Fatalf("decide: %q", u.note)
	}
	g(main, "branch", "-f", "schema", "schema~1") // someone else moved it
	moved := g(main, "rev-parse", "schema")
	u.apply(0)
	if !u.hand || !strings.Contains(u.note, "NOT merged: the branch moved") {
		t.Errorf("note %q", u.note)
	}
	if now := g(main, "rev-parse", "schema"); now != moved {
		t.Errorf("schema is at %s, want %s (the other change)", now, moved)
	}
}

func TestBareConflict(t *testing.T) {
	main, _, g := bareRepo(t, true)
	mainHead := g(main, "rev-parse", "main")
	before := g(main, "rev-parse", "schema")
	u, _ := bareUpdate(t, main, "schema")
	u.decide(main, "main", mainHead, false)
	if u.action != "" || !u.hand || !strings.HasPrefix(u.note, "CONFLICTS with main in 1 file") {
		t.Fatalf("decide: %q %q", u.action, u.note)
	}
	text := strings.Join(u.detail, "\n")
	if !strings.Contains(text, "conflict: schema.sql") || !strings.Contains(text, "worktree add") || !strings.Contains(text, "worktree remove") {
		t.Errorf("detail:\n%s", text)
	}
	if now := g(main, "rev-parse", "schema"); now != before {
		t.Error("a conflict moved the branch")
	}
}

// web, the only checkout, sits on core: an update merges core into it,
// and web's stack reads schema core web.
func TestWorktreeOnBareBranch(t *testing.T) {
	main, web, g := bareRepo(t, false)
	wts, _ := stack.List(main)
	view, err := loadStacks(main, "main", wts)
	if err != nil {
		t.Fatal(err)
	}
	g(web, "merge", "-q", "--no-edit", "main") // web takes main straight in
	head := g(web, "rev-parse", "HEAD")
	gs := gitState(stack.Worktree{Root: web, Branch: "web", Head: head}, main, "main", g(main, "rev-parse", "main"), 100, view)
	if gs.update.Parent != "core" || strings.Join(gs.stack, " ") != "schema core web" || gs.behind != 0 {
		t.Errorf("web: parent %s, stack %v, behind main %d", gs.update.Parent, gs.stack, gs.behind)
	}
	if gs.update.Behind != 0 || gs.update.Action != "" {
		t.Errorf("web against core: %+v", gs.update)
	}
	b := bareState(view.st.Bare[1], view, main, "main", g(main, "rev-parse", "main"))
	if view.st.Bare[1].Name != "schema" || b.update.Behind != 1 || b.update.Action != "merge" {
		t.Errorf("schema: %+v %+v", view.st.Bare[1], b.update)
	}
}
