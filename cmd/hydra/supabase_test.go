package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/plan"
)

func gitRepo(t *testing.T) gitx.Checkout {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	os.MkdirAll(filepath.Join(dir, "supabase"), 0o755)
	os.WriteFile(filepath.Join(dir, "supabase", ".gitignore"), []byte(".temp\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".env.local\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "init")
	co, err := gitx.Find(dir)
	if err != nil {
		t.Fatal(err)
	}
	return co
}

func TestTidySupabaseUndoesOnlyTheCLIsEdit(t *testing.T) {
	co := gitRepo(t)
	path := filepath.Join(co.Root, "supabase", ".gitignore")

	os.WriteFile(path, []byte(".temp\n.link.json\n..link.json.*.tmp\n.supabase.lock.*.tmp\n.supabase.lock.lease\n"), 0o644)
	if _, err := tidySupabase(co); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != ".temp\n" {
		t.Errorf("CLI lines kept: %q", b)
	}
	exclude, _ := os.ReadFile(filepath.Join(co.CommonDir, "info", "exclude"))
	if !strings.Contains(string(exclude), "**/supabase/supabase.lock\n") {
		t.Errorf("exclude = %q", exclude)
	}
	if _, err := tidySupabase(co); err != nil { // idempotent
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(filepath.Join(co.CommonDir, "info", "exclude")); string(again) != string(exclude) {
		t.Errorf("exclude grew on a second run:\n%s", again)
	}

	// Someone's own edit stays, CLI lines and all.
	mine := ".temp\nmy-file\n.link.json\n"
	os.WriteFile(path, []byte(mine), 0o644)
	if _, err := tidySupabase(co); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != mine {
		t.Errorf("a user's edit was undone: %q", b)
	}
}

// The main checkout's Supabase is started by the stack file's own command,
// which hydra doesn't wrap, and its start appends to supabase/.gitignore
// there too: main's router puts the file back when it changes, and
// `hydra update` does before it checks that main is clean.
func TestMainGetsSupabaseGitignoreBack(t *testing.T) {
	co := gitRepo(t)
	if !co.IsMain() {
		t.Fatal("gitRepo is meant to be a main checkout")
	}
	path := filepath.Join(co.Root, "supabase", ".gitignore")
	edited := ".temp\n.link.json\n.supabase.lock.lease\n"

	os.WriteFile(path, []byte(edited), 0o644)
	w := &gitignoreWatch{co: co}
	w.look()
	if b, _ := os.ReadFile(path); string(b) != ".temp\n" {
		t.Errorf("the router left the CLI's lines in main: %q", b)
	}
	os.WriteFile(path, []byte(edited), 0o644) // the next start
	future := time.Now().Add(time.Minute)
	os.Chtimes(path, future, future)
	w.look()
	if b, _ := os.ReadFile(path); string(b) != ".temp\n" {
		t.Errorf("a second start's lines stayed: %q", b)
	}

	st, err := config.Parse([]byte("processes:\n  supabase:\n    command: supabase start\n    x-hydra: { kind: supabase, port: 44321 }\n"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte(edited), 0o644)
	ms := &mainState{target: &target{checkout: co, stack: st}, Dirty: []string{"api/main.go", "supabase/.gitignore"}}
	if got := withoutCLIEdit(ms, true); strings.Join(got, ",") != "api/main.go" {
		t.Errorf("dry run: dirty = %v", got)
	}
	if b, _ := os.ReadFile(path); string(b) != edited {
		t.Errorf("a dry run changed the file: %q", b)
	}
	if got := withoutCLIEdit(ms, false); strings.Join(got, ",") != "api/main.go" {
		t.Errorf("dirty = %v", got)
	}
	if b, _ := os.ReadFile(path); string(b) != ".temp\n" {
		t.Errorf("update left the CLI's lines in main: %q", b)
	}

	// Someone's own edit is a change like any other.
	os.WriteFile(path, []byte(".temp\nmine\n"), 0o644)
	if got := withoutCLIEdit(ms, false); len(got) != 2 {
		t.Errorf("a person's edit was dropped from the uncommitted files: %v", got)
	}
}

func TestPrepareSupabaseRefusesWithoutStackMode(t *testing.T) {
	t.Setenv("SUPABASE_EXPERIMENTAL_STACK", "")
	err := prepareSupabase(nil)
	if err == nil || !strings.Contains(err.Error(), "main checkout's database") {
		t.Errorf("err = %v", err)
	}
}

func TestEnvLocalBlock(t *testing.T) {
	in := "A=1\n" + envBegin + "\nX=1\n" + envEnd + "\nB=2\n"
	if got := withoutBlock(in); got != "A=1\nB=2\n" {
		t.Errorf("withoutBlock = %q", got)
	}
	if got := withoutBlock("A=1\n"); got != "A=1\n" {
		t.Errorf("no block: %q", got)
	}
}

func TestSyncEnvLocalNeverWritesThroughALink(t *testing.T) {
	co := gitRepo(t)
	st, err := config.Parse([]byte("x-hydra: { slot_stride: 200 }\nprocesses:\n  supabase: { command: x, x-hydra: { kind: supabase, paths: [\"s/**\"] } }\n"))
	if err != nil {
		t.Fatal(err)
	}
	tg := &target{checkout: co, stack: st}
	p, err := plan.Build(st, plan.Request{Slot: 1, Changed: []string{"s/x"}})
	if err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(co.Root, ".env.local")

	// A file of its own gets the block, and loses it again.
	os.WriteFile(envPath, []byte("MINE=1\n"), 0o644)
	if err := syncEnvLocal(tg, p); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(envPath); !strings.Contains(string(b), "MINE=1\n"+envBegin) || !strings.Contains(string(b), "SUPABASE_API_PORT=54521") {
		t.Errorf("block missing: %q", b)
	}
	off, _ := plan.Build(st, plan.Request{Slot: 1})
	if err := syncEnvLocal(tg, off); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(envPath); string(b) != "MINE=1\n" {
		t.Errorf("block not removed: %q", b)
	}

	// A link to another checkout's file is left alone.
	other := filepath.Join(t.TempDir(), "main.env.local")
	os.WriteFile(other, []byte("MAIN=1\n"), 0o644)
	os.Remove(envPath)
	os.Symlink(other, envPath)
	if err := syncEnvLocal(tg, p); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(other); string(b) != "MAIN=1\n" {
		t.Errorf("wrote through the link: %q", b)
	}
}

// What reads a worktree's own database waits for the mark its start leaves.
func TestWaitPreparedReturnsWhenTheDatabaseIsMarkedReady(t *testing.T) {
	old := preparedPoll
	preparedPoll = 5 * time.Millisecond
	defer func() { preparedPoll = old }()

	root := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- waitPrepared(root, "supabase", os.Getppid()) }()
	select {
	case err := <-done:
		t.Fatalf("returned before the database was ready: %v", err)
	case <-time.After(60 * time.Millisecond):
	}
	if err := writeFile(supabaseMarker(root), []byte("refs/heads/x\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting after the mark was written")
	}

	// The process-compose that started it is gone: so is the reason to wait.
	if err := waitPrepared(t.TempDir(), "supabase", -1); err == nil {
		t.Error("kept waiting for a stack that is gone")
	}
}

// An update that starts the worktree's own database forgets that an earlier
// run had it ready; one that leaves the database alone doesn't.
func TestTheReadyMarkIsForgottenWhenTheDatabaseIsAboutToStart(t *testing.T) {
	co := gitRepo(t)
	st, err := config.Parse([]byte("x-hydra: { slot_stride: 200 }\nprocesses:\n  supabase: { command: x, x-hydra: { kind: supabase, paths: [\"s/**\"] } }\n"))
	if err != nil {
		t.Fatal(err)
	}
	tg := &target{checkout: co, stack: st}
	p, err := plan.Build(st, plan.Request{Slot: 1, Changed: []string{"s/x"}})
	if err != nil {
		t.Fatal(err)
	}
	marker := supabaseMarker(co.Root)
	mark := func() { t.Helper(); writeFile(marker, []byte("refs/heads/x\n")) }
	there := func() bool { _, err := os.Stat(marker); return err == nil }
	standIn := map[string]any{"supabase": map[string]any{"command": "idle", "namespace": "main"}}
	real := map[string]any{"supabase": map[string]any{"command": "supabase start"}}

	mark()
	forgetPrepared(tg, p, real, real) // an update of something else
	if !there() {
		t.Error("forgotten though the database keeps running")
	}
	forgetPrepared(tg, p, standIn, real) // main's database until now
	if there() {
		t.Error("kept, though the database is about to start")
	}
	mark()
	forgetPrepared(tg, p, nil, nil) // the stack itself starts
	if there() {
		t.Error("kept across a start of the stack")
	}
	// A worktree on main's database has no mark to forget, and none is made.
	off, _ := plan.Build(st, plan.Request{Slot: 1})
	mark()
	forgetPrepared(tg, off, nil, nil)
	if !there() {
		t.Error("removed for a worktree that doesn't run the database")
	}
}

// A migration that fails on a running database is only in the watcher's
// log, unless it is left where `hydra status` reads it.
func TestMigrationsThatDidNotApplyAreAProblem(t *testing.T) {
	root := t.TempDir()
	if pr := migrationsProblem(root, "supabase"); pr != nil {
		t.Fatalf("a problem with nothing recorded: %+v", pr)
	}
	said := "Connecting to local database...\n\n  \nApplying migration 20260101000000_x.sql...\nERROR: syntax error at or near \"tabel\" (SQLSTATE 42601)\nAt statement: 0\ncreate tabel x()\n"
	if err := writeFile(migrationsFailed(root), lastLines([]byte(said), 3)); err != nil {
		t.Fatal(err)
	}
	pr := migrationsProblem(root, "supabase")
	if pr == nil || !pr.Blocks {
		t.Fatalf("pr = %+v", pr)
	}
	for _, want := range []string{"supabase here: the migrations in supabase/migrations didn't all apply", "    | ERROR: syntax error at or near \"tabel\" (SQLSTATE 42601)", "    | create tabel x()"} {
		if !strings.Contains(pr.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, pr.Text)
		}
	}
	if strings.Contains(pr.Text, "Connecting to local database") {
		t.Errorf("more than the last lines:\n%s", pr.Text)
	}
	if !strings.Contains(pr.Fix, "correct the migration file") {
		t.Errorf("fix = %q", pr.Fix)
	}
}
