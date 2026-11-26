package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// What `gh pr list --json ...` prints, cut down: a stack's PRs, one merged,
// a draft, a branch with an old closed PR and a new open one, a fork's PR
// with the same branch name, and a branch nobody asked about.
const ghFixture = `[
 {"number":10266,"state":"OPEN","isDraft":false,"headRefName":"ring-web","baseRefName":"ring-core","reviewDecision":"REVIEW_REQUIRED","url":"https://github.com/o/r/pull/10266",
  "statusCheckRollup":[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"},{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SKIPPED"},{"__typename":"StatusContext","state":"SUCCESS"}]},
 {"number":10265,"state":"OPEN","isDraft":true,"headRefName":"ring-core","baseRefName":"ring-schema","reviewDecision":"","url":"https://github.com/o/r/pull/10265",
  "statusCheckRollup":[{"__typename":"CheckRun","status":"IN_PROGRESS","conclusion":""}]},
 {"number":10264,"state":"MERGED","isDraft":false,"headRefName":"ring-schema","baseRefName":"main","reviewDecision":"APPROVED","url":"https://github.com/o/r/pull/10264",
  "statusCheckRollup":[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"FAILURE"}]},
 {"number":900,"state":"CLOSED","isDraft":false,"headRefName":"cart","baseRefName":"main","reviewDecision":"","url":"https://github.com/o/r/pull/900","statusCheckRollup":[]},
 {"number":950,"state":"OPEN","isDraft":false,"headRefName":"cart","baseRefName":"main","reviewDecision":"CHANGES_REQUESTED","url":"https://github.com/o/r/pull/950",
  "statusCheckRollup":[{"__typename":"StatusContext","state":"FAILURE"}]},
 {"number":951,"state":"OPEN","isDraft":false,"headRefName":"api","baseRefName":"main","isCrossRepository":true,"reviewDecision":"","url":"https://github.com/fork/r/pull/951","statusCheckRollup":[]},
 {"number":1,"state":"OPEN","isDraft":false,"headRefName":"someone-else","baseRefName":"main","reviewDecision":"","url":"u","statusCheckRollup":[]}
]`

func TestParsePRs(t *testing.T) {
	want := map[string]bool{"ring-web": true, "ring-core": true, "ring-schema": true, "cart": true, "api": true}
	got, err := parsePRs([]byte(ghFixture), want)
	if err != nil {
		t.Fatal(err)
	}
	check := func(branch string, pr snapPR, short, long string) {
		t.Helper()
		if got[branch] != pr {
			t.Errorf("%s = %+v, want %+v", branch, got[branch], pr)
		}
		p := got[branch]
		if p.Short() != short || p.Long() != long {
			t.Errorf("%s: %q / %q, want %q / %q", branch, p.Short(), p.Long(), short, long)
		}
	}
	check("ring-web", snapPR{Number: 10266, State: "open", Review: "review required", Checks: "passing", URL: "https://github.com/o/r/pull/10266", Base: "ring-core"},
		"#10266 ✓", "#10266 open · review required · checks passing")
	check("ring-core", snapPR{Number: 10265, State: "draft", Checks: "pending", URL: "https://github.com/o/r/pull/10265", Base: "ring-schema"},
		"#10265 draft", "#10265 draft · checks pending")
	// Merged: its review and checks don't matter any more.
	check("ring-schema", snapPR{Number: 10264, State: "merged", Review: "approved", Checks: "failing", URL: "https://github.com/o/r/pull/10264", Base: "main"},
		"#10264 merged", "#10264 merged")
	// The open PR wins over the old closed one.
	check("cart", snapPR{Number: 950, State: "open", Review: "changes requested", Checks: "failing", URL: "https://github.com/o/r/pull/950", Base: "main"},
		"#950 ✗ checks", "#950 open · changes requested · checks failing")
	if _, ok := got["api"]; ok {
		t.Error("a fork's PR with the same branch name was taken for the branch's")
	}
	if _, ok := got["someone-else"]; ok {
		t.Error("a branch nobody asked about was kept")
	}
	if schema := got["ring-schema"]; !schema.Done() || (&snapPR{State: "draft"}).Done() {
		t.Error("Done is off")
	}
	if _, err := parsePRs([]byte("not json"), want); err == nil {
		t.Error("gh's garbage parsed")
	}
}

func TestChecksRollUp(t *testing.T) {
	for _, tc := range []struct {
		checks []ghCheck
		want   string
	}{
		{nil, ""},
		{[]ghCheck{{Status: "COMPLETED", Conclusion: "SUCCESS"}, {Status: "COMPLETED", Conclusion: "NEUTRAL"}}, "passing"},
		{[]ghCheck{{Status: "COMPLETED", Conclusion: "SUCCESS"}, {Status: "QUEUED"}}, "pending"},
		{[]ghCheck{{Status: "QUEUED"}, {Status: "COMPLETED", Conclusion: "TIMED_OUT"}}, "failing"},
		{[]ghCheck{{State: "PENDING"}}, "pending"},
		{[]ghCheck{{State: "ERROR"}}, "failing"},
		{[]ghCheck{{Status: "COMPLETED", Conclusion: "CANCELLED"}, {Status: "COMPLETED", Conclusion: "SUCCESS"}}, "passing"},
	} {
		if got := checksOf(tc.checks); got != tc.want {
			t.Errorf("checksOf(%+v) = %q, want %q", tc.checks, got, tc.want)
		}
	}
	if s := (&snapPR{Number: 3, State: "open", Review: "changes requested"}).Short(); s != "#3 ✗ review" {
		t.Errorf("Short = %q", s)
	}
	if s := (&snapPR{State: "merged", MergedLocally: true}).Short(); s != "merged" {
		t.Errorf("Short = %q", s)
	}
}

// A branch merged on GitHub with a merge commit is in origin/main, off its
// first-parent line; one just cut from main, or still open, isn't merged.
func TestMergedByAncestry(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(file string) string {
		os.WriteFile(filepath.Join(dir, file), []byte(file), 0o644)
		git("add", file)
		git("commit", "-qm", file)
		return git("rev-parse", "HEAD")
	}
	git("init", "-q", "-b", "main")
	commit("a")
	fresh := git("rev-parse", "HEAD") // a branch cut here, nothing of its own
	git("checkout", "-q", "-b", "done")
	done := commit("b")
	git("checkout", "-q", "main")
	git("checkout", "-q", "-b", "open")
	open := commit("c")
	git("checkout", "-q", "main")
	commit("d")
	git("merge", "-q", "--no-ff", "--no-edit", "done")
	commit("e")
	// origin's main, as a fetch leaves it
	git("update-ref", "refs/remotes/origin/main", "main")
	target := mergeTarget(dir, "main")
	if target != "origin/main" {
		t.Fatalf("target = %q", target)
	}
	if !mergedByAncestry(dir, done, target) {
		t.Error("a branch merged with a merge commit isn't merged")
	}
	if mergedByAncestry(dir, fresh, target) {
		t.Error("a branch cut from main with no commit of its own counts as merged")
	}
	if mergedByAncestry(dir, open, target) {
		t.Error("an open branch counts as merged")
	}
	if mergedByAncestry(dir, git("rev-parse", "main"), target) || mergedByAncestry(dir, "", target) {
		t.Error("main itself, or no commit, counts as merged")
	}
	git("update-ref", "-d", "refs/remotes/origin/main")
	if mergeTarget(dir, "main") != "main" {
		t.Error("without origin, the local base isn't the target")
	}
}

// Without gh, logged out, or with no remote: no PRs, a reason, no error
// anywhere else.
func TestNoGhIsHarmless(t *testing.T) {
	co := gitRepo(t)
	t.Setenv("HYDRA_CACHE_DIR", t.TempDir())
	ctx := context.Background()

	t.Setenv("HYDRA_GH", filepath.Join(t.TempDir(), "no-such-gh"))
	if _, err := fetchPRs(ctx, co.Root, []string{"x"}); err == nil || !strings.Contains(err.Error(), "gh isn't installed") {
		t.Errorf("missing gh: %v", err)
	}
	t.Setenv("HYDRA_GH", "off")
	if _, err := fetchPRs(ctx, co.Root, []string{"x"}); err == nil || !strings.Contains(err.Error(), "off") {
		t.Errorf("HYDRA_GH=off: %v", err)
	}

	fake := filepath.Join(t.TempDir(), "gh")
	os.WriteFile(fake, []byte("#!/bin/sh\necho 'To get started with GitHub CLI, please run:  gh auth login' >&2\nexit 4\n"), 0o755)
	t.Setenv("HYDRA_GH", fake)
	if _, err := fetchPRs(ctx, co.Root, []string{"x"}); err == nil || !strings.Contains(err.Error(), "no remote on a server") {
		t.Errorf("no remote: %v", err)
	}
	exec.Command("git", "-C", co.Root, "remote", "add", "origin", "https://example.invalid/o/r.git").Run()
	if _, err := fetchPRs(ctx, co.Root, []string{"x"}); err == nil || err.Error() != "gh isn't logged in: gh auth login" {
		t.Errorf("logged out: %v", err)
	}

	// A slow GitHub: the timeout, then the reason.
	os.WriteFile(fake, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755)
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if _, err := fetchPRs(short, co.Root, []string{"x"}); err == nil || !strings.Contains(err.Error(), "didn't answer") {
		t.Errorf("slow: %v", err)
	}

	// A source with no gh gives no PR and says why; it keeps what it knew.
	os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s' '"+strings.ReplaceAll(ghFixture, "\n", "")+"'\n"), 0o755)
	p := newPRSource(co.Root)
	p.refresh([]string{"cart"}, nil, "main", true, time.Second, nil)
	if pr := p.of("cart"); pr == nil || pr.Number != 950 {
		t.Fatalf("cart = %+v", pr)
	}
	if src := p.source(); src.Error != "" || src.At == nil {
		t.Errorf("source = %+v", src)
	}
	t.Setenv("HYDRA_GH", "off")
	p.refresh([]string{"cart"}, nil, "main", true, time.Second, nil)
	if pr := p.of("cart"); pr == nil || pr.Number != 950 {
		t.Errorf("what was known went away: %+v", pr)
	}
	if src := p.source(); !strings.Contains(src.Error, "off") {
		t.Errorf("source = %+v", src)
	}
	// The disk cache gives the next one the same, before it asks.
	if pr := newPRSource(co.Root).of("cart"); pr == nil || pr.Number != 950 {
		t.Errorf("from disk: %+v", pr)
	}
}

// The snapshot's PR fields are additions: an older reader's fields are all
// still there, and the new ones are left out when there is nothing to say.
func TestSnapshotPRFieldsAreAdditive(t *testing.T) {
	w := snapWorktree{Branch: "cart", OutOfStep: []snapSkew{}, Services: []snapService{}, Problems: []snapProblem{}}
	data, _ := json.Marshal(w)
	if strings.Contains(string(data), `"pr"`) {
		t.Errorf("no PR, yet: %s", data)
	}
	w.PR = &snapPR{Number: 950, State: "merged", URL: "u", MergedLocally: true}
	data, _ = json.Marshal(w)
	if !strings.Contains(string(data), `"pr":{"number":950,"state":"merged","url":"u","mergedLocally":true}`) {
		t.Errorf("pr = %s", data)
	}
	snap := snapshot{Version: snapshotVersion, PRs: &snapPRSource{Error: "gh isn't installed"}}
	data, _ = json.Marshal(snap)
	if snapshotVersion != 1 || !strings.Contains(string(data), `"prs":{"error":"gh isn't installed"}`) {
		t.Errorf("snapshot = %s", data)
	}
}
