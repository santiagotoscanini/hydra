package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	if err := tidySupabase(co); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != ".temp\n" {
		t.Errorf("CLI lines kept: %q", b)
	}
	exclude, _ := os.ReadFile(filepath.Join(co.CommonDir, "info", "exclude"))
	if !strings.Contains(string(exclude), "**/supabase/supabase.lock\n") {
		t.Errorf("exclude = %q", exclude)
	}
	if err := tidySupabase(co); err != nil { // idempotent
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(filepath.Join(co.CommonDir, "info", "exclude")); string(again) != string(exclude) {
		t.Errorf("exclude grew on a second run:\n%s", again)
	}

	// Someone's own edit stays, CLI lines and all.
	mine := ".temp\nmy-file\n.link.json\n"
	os.WriteFile(path, []byte(mine), 0o644)
	if err := tidySupabase(co); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != mine {
		t.Errorf("a user's edit was undone: %q", b)
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
