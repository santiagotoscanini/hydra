package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The actions run hydra itself: the tests build it once.
var hydraBin struct {
	once sync.Once
	dir  string
	err  error
}

func builtHydra(t *testing.T) string {
	t.Helper()
	hydraBin.once.Do(func() {
		hydraBin.dir, hydraBin.err = os.MkdirTemp("", "hydra-test-bin-")
		if hydraBin.err != nil {
			return
		}
		out, err := exec.Command("go", "build", "-o", hydraBin.dir, ".").CombinedOutput()
		if err != nil {
			hydraBin.err = err
			hydraBin.dir = string(out)
		}
	})
	if hydraBin.err != nil {
		t.Fatalf("building hydra: %v\n%s", hydraBin.err, hydraBin.dir)
	}
	return filepath.Join(hydraBin.dir, "hydra")
}

func canon(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func TestMain(m *testing.M) {
	// No test reaches GitHub, or writes to the user's cache.
	cache, err := os.MkdirTemp("", "hydra-test-cache")
	if err != nil {
		panic(err)
	}
	os.Setenv("HYDRA_CACHE_DIR", cache)
	os.Setenv("HYDRA_GH", "off")
	// A machine with no git identity (CI) can still commit and merge, in
	// the tests and in the hydra they run.
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		if os.Getenv(k) == "" {
			os.Setenv(k, map[bool]string{true: "t@t", false: "t"}[strings.HasSuffix(k, "EMAIL")])
		}
	}
	code := m.Run()
	os.RemoveAll(cache)
	if hydraBin.err == nil && hydraBin.dir != "" {
		os.RemoveAll(hydraBin.dir)
	}
	os.Exit(code)
}

// stackedRepo is mcpRepo's main checkout with a stack of three worktrees
// (a, b on a, c on b) and one on main of its own, d, whose change to
// shared.txt conflicts with main's. main has moved on since they were cut.
func stackedRepo(t *testing.T) (main string, wt func(string) string) {
	t.Helper()
	main, feature := mcpRepo(t)
	dir := filepath.Dir(main)
	wt = func(name string) string { return filepath.Join(dir, name) }
	run := func(in string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", in}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	commit := func(in, file, text string) {
		t.Helper()
		os.MkdirAll(filepath.Dir(filepath.Join(in, file)), 0o755)
		os.WriteFile(filepath.Join(in, file), []byte(text), 0o644)
		run(in, "add", "-A")
		run(in, "commit", "-q", "-m", file)
	}
	run(main, "worktree", "remove", feature)
	commit(main, "shared.txt", "base\n")
	run(main, "worktree", "add", "-q", "-b", "a", wt("a"))
	commit(wt("a"), "apps/api/a1.go", "a")
	run(main, "worktree", "add", "-q", "-b", "b", wt("b"), "a")
	commit(wt("b"), "apps/api/b1.go", "b")
	run(main, "worktree", "add", "-q", "-b", "c", wt("c"), "b")
	commit(wt("c"), "apps/api/c1.go", "c")
	run(main, "worktree", "add", "-q", "-b", "d", wt("d"))
	commit(wt("d"), "shared.txt", "d's\n")
	commit(main, "shared.txt", "main's\n")
	return main, wt
}

func TestMCPWorktreesInUpdateOrder(t *testing.T) {
	main, wt := stackedRepo(t)
	os.WriteFile(filepath.Join(wt("d"), "apps", "api", "a.go"), []byte("edited"), 0o644)
	r, _ := session(t, wt("b"), toolCall(1, "worktrees", `{}`))
	text, isErr := toolText(t, r["1"])
	if isErr {
		t.Fatal(text)
	}
	data := r["1"]["result"].(map[string]any)["structuredContent"].(map[string]any)
	if m := data["main"].(map[string]any); m["path"] != canon(main) || m["stackUp"] != false || m["branch"] != "main" {
		t.Errorf("main: %v", m)
	}
	var got []string
	for _, w := range data["worktrees"].([]any) {
		e := w.(map[string]any)
		got = append(got, strings.Join([]string{e["name"].(string), e["parent"].(string), e["next"].(string)}, ":"))
	}
	// a first, then what is stacked on it, in order; d, on main, after.
	want := "a:main:merge b:a:nothing c:b:nothing d:main:skip"
	if strings.Join(got, " ") != want {
		t.Errorf("order:\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	d := data["worktrees"].([]any)[3].(map[string]any)
	if d["uncommitted"] != float64(1) || len(d["conflicts"].([]any)) != 1 || d["conflicts"].([]any)[0] != "shared.txt" || d["behind"] != float64(1) {
		t.Errorf("d: %v", d)
	}
	for _, line := range []string{" 1. main ", " 2. a ", " 3. └ b ", " 4.   └ c ", " 5. d ", "on a · 1 ahead · nothing to do", "and merging it conflicts in 1 file: shared.txt"} {
		if !strings.Contains(text, line) {
			t.Errorf("summary lacks %q:\n%s", line, text)
		}
	}
}

func TestMCPUpdateWorktree(t *testing.T) {
	main, wt := stackedRepo(t)
	exe := builtHydra(t)
	// From the main checkout: these tools aren't refused there.
	r, _ := sessionWith(t, main, exe,
		toolCall(1, "update_worktree", `{"worktree":"a"}`),
		toolCall(2, "update_worktree", `{"worktree":"main"}`),
		toolCall(3, "update_worktree", `{"worktree":"nope"}`),
	)
	if text, isErr := toolText(t, r["1"]); isErr || !strings.Contains(text, "main merged in (1 commit)") {
		t.Errorf("clean merge: %v\n%s", isErr, text)
	}
	if out, _ := gitOut(wt("a"), "log", "-1", "--format=%s"); !strings.HasPrefix(out, "Merge branch 'main'") {
		t.Errorf("a's last commit: %q", out)
	}
	if text, isErr := toolText(t, r["2"]); !isErr || !strings.Contains(text, "is the main checkout; update_main updates it") {
		t.Errorf("main as a worktree: %v %s", isErr, text)
	}
	if text, isErr := toolText(t, r["3"]); !isErr || !strings.Contains(text, `no worktree named "nope"; the worktrees are: a, b, c, d`) {
		t.Errorf("an unknown worktree: %v %s", isErr, text)
	}

	// A conflict: every file named, the way on, and the worktree as it was.
	before, _ := gitOut(wt("d"), "rev-parse", "HEAD")
	r, _ = sessionWith(t, wt("b"), exe, toolCall(1, "update_worktree", `{"worktree":"`+wt("d")+`"}`))
	text, isErr := toolText(t, r["1"])
	for _, s := range []string{"CONFLICTS with main in 1 file", "Merging main conflicts in 1 file:\n  shared.txt", "git merge main", "since"} {
		if !isErr || !strings.Contains(text, s) {
			t.Errorf("conflict: %v, lacks %q:\n%s", isErr, s, text)
		}
	}
	if st, _ := gitOut(wt("d"), "status", "--porcelain"); st != "" {
		t.Errorf("d was left changed:\n%s", st)
	}
	if head, _ := gitOut(wt("d"), "rev-parse", "HEAD"); head != before {
		t.Errorf("d moved: %s → %s", before, head)
	}

	// Resolved by hand, then update_worktree with since.
	merge := exec.Command("git", "-C", wt("d"), "merge", "main")
	merge.Run()
	os.WriteFile(filepath.Join(wt("d"), "shared.txt"), []byte("both\n"), 0o644)
	for _, args := range [][]string{{"add", "shared.txt"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "--no-edit", "-q"}} {
		if out, err := exec.Command("git", append([]string{"-C", wt("d")}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	r, _ = sessionWith(t, main, exe,
		toolCall(1, "update_worktree", `{"worktree":"d","since":"`+before+`"}`),
		toolCall(2, "update_worktree", `{"worktree":"d","since":"main"}`),
	)
	if text, isErr := toolText(t, r["1"]); isErr || !strings.Contains(text, "up to date") || !strings.Contains(text, "nothing to restart since") {
		t.Errorf("after the merge by hand: %v\n%s", isErr, text)
	}
	if text, isErr := toolText(t, r["2"]); isErr {
		t.Errorf("since main, which d now descends from: %s", text)
	}
}

func TestMCPUpdateMainDryRun(t *testing.T) {
	main, wt := stackedRepo(t)
	exe := builtHydra(t)
	head, _ := gitOut(main, "rev-parse", "HEAD")
	r, _ := sessionWith(t, wt("c"), exe, toolCall(1, "update_main", `{"dry_run":true}`))
	text, isErr := toolText(t, r["1"])
	for _, s := range []string{"dry run", "main has no upstream, nothing to pull", "main's stack isn't running", "Run `hydra update` to do it."} {
		if isErr || !strings.Contains(text, s) {
			t.Errorf("update_main dry run: %v, lacks %q:\n%s", isErr, s, text)
		}
	}
	if strings.Contains(text, "worktrees") {
		t.Errorf("a main-only update listed the worktrees:\n%s", text)
	}
	if now, _ := gitOut(main, "rev-parse", "HEAD"); now != head {
		t.Error("a dry run moved main")
	}
}

func TestMCPPrompts(t *testing.T) {
	main, _ := mcpRepo(t)
	r, _ := session(t, main,
		request(1, "initialize", `{"protocolVersion":"2025-06-18"}`),
		request(2, "prompts/list", `{}`),
		request(3, "prompts/get", `{"name":"update_all"}`),
		request(4, "prompts/get", `{"name":"update_all","arguments":{"instructions":"leave fla-709 out"}}`),
		request(5, "prompts/get", `{"name":"nope"}`),
	)
	caps := r["1"]["result"].(map[string]any)["capabilities"].(map[string]any)
	if _, ok := caps["prompts"]; !ok {
		t.Errorf("no prompts capability: %v", caps)
	}
	prompts := r["2"]["result"].(map[string]any)["prompts"].([]any)
	if len(prompts) != 1 || prompts[0].(map[string]any)["name"] != "update_all" {
		t.Fatalf("prompts: %v", prompts)
	}
	text := func(id string) string {
		msgs := r[id]["result"].(map[string]any)["messages"].([]any)
		m := msgs[0].(map[string]any)
		if m["role"] != "user" {
			t.Errorf("role %v", m["role"])
		}
		return m["content"].(map[string]any)["text"].(string)
	}
	got := text("3")
	for _, s := range []string{"Call worktrees", "update_main with dry_run: true", "update_worktree", "git merge --abort", "Never push", "since"} {
		if !strings.Contains(got, s) {
			t.Errorf("the workflow lacks %q", s)
		}
	}
	if got := text("4"); !strings.HasSuffix(got, "leave fla-709 out") {
		t.Errorf("the developer's instructions aren't last:\n%s", got[len(got)-200:])
	}
	if c := r["5"]["error"].(map[string]any)["code"]; c != float64(rpcInvalidParams) {
		t.Errorf("an unknown prompt: %v", r["5"])
	}

	// Outside hydra: none.
	r, _ = session(t, t.TempDir(), request(1, "prompts/list", `{}`), request(2, "prompts/get", `{"name":"update_all"}`))
	if p := r["1"]["result"].(map[string]any)["prompts"].([]any); len(p) != 0 {
		t.Errorf("prompts outside a hydra checkout: %v", p)
	}
	if _, ok := r["2"]["error"]; !ok {
		t.Errorf("prompts/get outside a hydra checkout: %v", r["2"])
	}
}

// The watch works out where a worktree stands again when its index or its
// merge changed, without a commit, but not more often than gitIndexGap.
func TestGitStateIsDueWhenTheWorktreeMoves(t *testing.T) {
	now := time.Now()
	mark := indexMark{index: now.Add(-time.Minute), size: 10}
	g := &wtGit{head: "h", mainHead: "m", parentHead: "p", update: &snapUpdate{Parent: "main"}, index: mark, at: now.Add(-10 * time.Second)}
	for _, c := range []struct {
		name                string
		g                   *wtGit
		head, main, parent  string
		mark                indexMark
		at                  time.Time
		wantDue, wantUrgent bool
	}{
		{"never looked", nil, "h", "m", "p", mark, now, true, false},
		{"nothing moved", g, "h", "m", "p", mark, now, false, false},
		{"a commit", g, "h2", "m", "p", mark, now, true, true},
		{"its parent moved", g, "h", "m", "p2", mark, now, true, true},
		{"main moved", g, "h", "m2", "p", mark, now, true, false},
		{"a merge begun", g, "h", "m", "p", indexMark{index: mark.index, size: 10, merge: true}, now, true, true},
		{"files staged", g, "h", "m", "p", indexMark{index: now, size: 12}, now, true, true},
		{"files staged a moment after the last look", g, "h", "m", "p", indexMark{index: now, size: 12}, g.at.Add(time.Second), false, false},
		{"two minutes on", g, "h", "m", "p", mark, now.Add(gitAge), true, false},
	} {
		due, urgent := gitDue(c.g, c.head, c.main, c.parent, c.mark, c.at)
		if due != c.wantDue || urgent != c.wantUrgent {
			t.Errorf("%s: due %v urgent %v, want %v %v", c.name, due, urgent, c.wantDue, c.wantUrgent)
		}
	}
}

func TestIndexMarkOfAWorktree(t *testing.T) {
	_, wt := mcpRepo(t)
	gitDir := gitDirOf(wt)
	if want, _ := gitOut(wt, "rev-parse", "--absolute-git-dir"); gitDir != want {
		t.Fatalf("gitDirOf: %q, want %q", gitDir, want)
	}
	before := markOf(gitDir)
	if before.index.IsZero() || before.merge {
		t.Fatalf("mark: %+v", before)
	}
	os.WriteFile(filepath.Join(gitDir, "MERGE_HEAD"), []byte("x\n"), 0o644)
	if after := markOf(gitDir); !after.merge || after == before {
		t.Errorf("a merge under way: %+v", after)
	}
}

// A branch whose PR is merged has nothing left to do: worktrees says so,
// and the update_all workflow skips it.
func TestMCPWorktreesSkipAMergedPR(t *testing.T) {
	main, wt := stackedRepo(t)
	exec.Command("git", "-C", main, "remote", "add", "origin", "https://example.invalid/o/r.git").Run()
	fake := filepath.Join(t.TempDir(), "gh")
	os.WriteFile(fake, []byte(`#!/bin/sh
echo '[{"number":5,"state":"MERGED","headRefName":"a","baseRefName":"main","url":"https://example.invalid/pull/5","statusCheckRollup":[]},
 {"number":6,"state":"OPEN","isDraft":true,"headRefName":"b","baseRefName":"a","url":"https://example.invalid/pull/6","statusCheckRollup":[]}]'
`), 0o755)
	t.Setenv("HYDRA_GH", fake)
	r, _ := session(t, wt("b"), toolCall(1, "worktrees", `{}`))
	text, isErr := toolText(t, r["1"])
	if isErr {
		t.Fatal(text)
	}
	ws := r["1"]["result"].(map[string]any)["structuredContent"].(map[string]any)["worktrees"].([]any)
	a, b := ws[0].(map[string]any), ws[1].(map[string]any)
	if a["next"] != "skip" || a["pr"].(map[string]any)["state"] != "merged" || !strings.Contains(a["note"].(string), "#5 merged, nothing left to do") {
		t.Errorf("a: %v", a)
	}
	if b["pr"].(map[string]any)["number"] != float64(6) || b["next"] != "nothing" {
		t.Errorf("b: %v", b)
	}
	if !strings.Contains(text, "PR #6 draft") || !strings.Contains(text, "SKIP: left alone: its PR is #5 merged") {
		t.Errorf("summary:\n%s", text)
	}
}
