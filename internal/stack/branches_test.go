package stack

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitIn runs git in d with a fixed identity and returns what it printed.
func gitIn(t *testing.T, d string, args ...string) string {
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

// ringRepo builds the PR stack main <- schema <- core <- web with only web
// checked out (in its own worktree), and main merged straight into web
// after main moved on. Beside it: a backup of core from before a split, a
// prototype cut from schema, and a branch merged into main.
func ringRepo(t *testing.T) (main, web string) {
	t.Helper()
	dir := t.TempDir()
	main = filepath.Join(dir, "main")
	os.Mkdir(main, 0o755)
	g := func(args ...string) string { return gitIn(t, main, args...) }
	commit := func(msg string) { g("commit", "-q", "--allow-empty", "-m", msg) }
	g("init", "-q", "-b", "main")
	commit("root")
	g("branch", "merged")
	g("checkout", "-q", "-b", "schema")
	commit("schema")
	g("checkout", "-q", "-b", "core")
	commit("core 1")
	g("branch", "backup/core-pre-split")
	commit("core 2")
	g("checkout", "-q", "-b", "web")
	commit("web")
	g("checkout", "-q", "-b", "prototype", "schema")
	commit("prototype")
	g("checkout", "-q", "main")
	g("merge", "-q", "--ff-only", "merged")
	commit("main moves on")
	web = filepath.Join(dir, "web")
	g("worktree", "add", "-q", web, "web")
	gitIn(t, web, "merge", "-q", "--no-edit", "main")
	return main, web
}

func bareNames(s *Stacks) string {
	var out []string
	for _, b := range s.Bare {
		out = append(out, b.Name+"<"+b.Parent+"("+b.Source+")")
	}
	return strings.Join(out, " ")
}

func TestFindStacksByAncestry(t *testing.T) {
	main, web := ringRepo(t)
	wts, err := List(main)
	if err != nil {
		t.Fatal(err)
	}
	st, err := FindStacks(main, "main", wts)
	if err != nil {
		t.Fatal(err)
	}
	// Not the backup, the prototype (not under web) or the merged branch.
	if got, want := bareNames(st), "core<schema(ancestry) schema<main(ancestry)"; got != want {
		t.Errorf("bare = %s, want %s", got, want)
	}
	parent, chain, err := Place(web, Options{BaseRef: "main"}, st)
	if err != nil {
		t.Fatal(err)
	}
	if parent != "core" || strings.Join(chain, " ") != "schema core web" {
		t.Errorf("web: parent %s, chain %v; want core, [schema core web]", parent, chain)
	}
	// Without the stacks, as before: web sits on main.
	if parent, _, _ := Place(web, Options{BaseRef: "main"}, nil); parent != "main" {
		t.Errorf("without stacks: parent %s, want main", parent)
	}
	if got := strings.Join(st.BareChain("core", nil), " "); got != "schema core" {
		t.Errorf("core's chain: %s", got)
	}

	// schema takes main in: core doesn't contain it any more, but schema
	// only took a merge since a commit core has. Still the same stack.
	gitIn(t, main, "checkout", "-q", "schema")
	gitIn(t, main, "merge", "-q", "--no-edit", "main")
	gitIn(t, main, "checkout", "-q", "main")
	st, err = FindStacks(main, "main", wts)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bareNames(st), "core<schema(ancestry) schema<main(ancestry)"; got != want {
		t.Errorf("after schema merged main: bare = %s, want %s", got, want)
	}
}

// A branch merged into web from elsewhere isn't a chain with the stack: two
// branches under web that don't contain one another mean nothing is
// inferred.
func TestFindStacksNotAChain(t *testing.T) {
	main, web := ringRepo(t)
	gitIn(t, web, "merge", "-q", "--no-edit", "prototype")
	wts, _ := List(main)
	st, err := FindStacks(main, "main", wts)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Bare) != 0 {
		t.Errorf("bare = %s, want none", bareNames(st))
	}
}

// gh-stack's file says the stack, even where ancestry would say otherwise:
// here core was amended, so it no longer contains schema.
func TestFindStacksFromGHStack(t *testing.T) {
	main, _ := ringRepo(t)
	gitIn(t, main, "checkout", "-q", "core")
	gitIn(t, main, "commit", "-q", "--amend", "--allow-empty", "-m", "core 2, amended")
	gitIn(t, main, "checkout", "-q", "main")
	file := `{"schemaVersion":1,"stacks":[
	  {"trunk":{"branch":"main"},"branches":[{"branch":"gone"},{"branch":"merged"},{"branch":"schema"},{"branch":"core"},{"branch":"web"},{"branch":"above"}]}]}`
	if err := os.WriteFile(filepath.Join(main, ".git", "gh-stack"), []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	wts, _ := List(main)
	st, err := FindStacks(main, "main", wts)
	if err != nil {
		t.Fatal(err)
	}
	// gone doesn't exist and merged is in main: schema sits on main.
	if got, want := bareNames(st), "core<schema(gh-stack) schema<main(gh-stack)"; got != want {
		t.Errorf("bare = %s, want %s", got, want)
	}
	if p, _ := st.Parent("web"); p != "core" {
		t.Errorf("web's parent = %s, want core", p)
	}
}

// The answer is cached until a branch moves.
func TestFindStacksCache(t *testing.T) {
	main, _ := ringRepo(t)
	wts, _ := List(main)
	a, _ := FindStacks(main, "main", wts)
	b, _ := FindStacks(main, "main", wts)
	if a != b {
		t.Error("asked twice with nothing moved: computed twice")
	}
	gitIn(t, main, "branch", "-f", "schema", "main") // schema merged: gone from the stack
	c, _ := FindStacks(main, "main", wts)
	if c == a {
		t.Fatal("a branch moved: the cached answer was kept")
	}
	if got := bareNames(c); strings.Contains(got, "schema") {
		t.Errorf("bare = %s: schema is in main now", got)
	}
}

// Copies of a stack's branches at the same commits (another name for each,
// made to move the stack) don't hide the stack: one name per commit is
// kept, the one that shares the worktree's branch's path.
func TestFindStacksWithCopiesOfItsBranches(t *testing.T) {
	main, web := ringRepo(t)
	g := func(args ...string) string { return gitIn(t, main, args...) }
	g("branch", "-m", "schema", "me/ring/schema")
	g("branch", "-m", "core", "me/ring/core")
	gitIn(t, web, "branch", "-m", "web", "me/ring/web")
	g("branch", "port/schema", "me/ring/schema")
	g("branch", "port/core", "me/ring/core")
	wts, err := List(main)
	if err != nil {
		t.Fatal(err)
	}
	st, err := FindStacks(main, "main", wts)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bareNames(st), "me/ring/core<me/ring/schema(ancestry) me/ring/schema<main(ancestry)"; got != want {
		t.Errorf("bare = %s, want %s", got, want)
	}
}
