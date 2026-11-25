package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santiagotoscanini/hydra/internal/render"
)

func TestMainsMigrations(t *testing.T) {
	files := map[string]string{"1": "1_a.sql", "2": "2_b.sql", "3": "3_branch.sql"}

	// A new database: main's two go in, the branch's own waits for the
	// migration step that follows the copy.
	got, err := mainsMigrations([]string{"1", "2"}, nil, files)
	if err != nil || strings.Join(got, " ") != "1_a.sql 2_b.sql" {
		t.Errorf("got %v, %v", got, err)
	}
	// A start that stopped after them: they are applied already, which the
	// CLI skips; the copy goes on.
	if _, err := mainsMigrations([]string{"1", "2"}, []string{"1", "2"}, files); err != nil {
		t.Errorf("resumed: %v", err)
	}
	// The branch is behind main: main's rows don't fit its schema.
	if _, err := mainsMigrations([]string{"1", "2", "4"}, nil, files); err == nil || !strings.Contains(err.Error(), "migrations this branch doesn't (4)") {
		t.Errorf("behind main: %v", err)
	}
	// The database has the branch's migration already: main's data can't go
	// in under it.
	if _, err := mainsMigrations([]string{"1"}, []string{"1", "3"}, files); err == nil || !strings.Contains(err.Error(), "already has migration 3") {
		t.Errorf("past main: %v", err)
	}
}

func TestMigrationFiles(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"20260101000000_items.sql", "20260102000000_more.sql", "README.md", "notes.sql", "1_x.sql.bak"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := migrationFiles(dir)
	if err != nil || len(got) != 2 || got["20260102000000"] != "20260102000000_more.sql" {
		t.Errorf("got %v, %v", got, err)
	}
	if got, err := migrationFiles(filepath.Join(dir, "none")); err != nil || len(got) != 0 {
		t.Errorf("no folder: %v, %v", got, err)
	}
}

// markSeed decides, before each start, whether it creates the stack: only
// then does the database start empty, to be filled from main's.
func TestMarkSeed(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, ".process-compose"), 0o755)
	pending := filepath.Join(root, render.SeedPending)
	stacks := func() []supabaseStack {
		return []supabaseStack{
			{ID: "feat", Root: root, Branch: "refs/heads/feat", Owner: "unavailable"},
			{ID: "old", Root: root, Branch: "refs/heads/old", Owner: "unavailable"},
		}
	}

	// The branch has a stack: it resumes, with its data.
	f := &fakeSupabase{stacks: stacks()}
	if err := markSeed(root, "refs/heads/feat", f.run); err != nil || exists(pending) {
		t.Fatalf("resume marked a copy: %v", err)
	}
	// Another branch's doesn't count: this start creates one.
	if err := markSeed(root, "refs/heads/new", f.run); err != nil || !exists(pending) {
		t.Fatalf("new stack not marked: %v", err)
	}
	// Still there from a start that didn't get as far as the copy: kept,
	// whatever the stacks.
	if err := markSeed(root, "refs/heads/feat", f.run); err != nil || !exists(pending) {
		t.Fatalf("pending copy dropped: %v", err)
	}
	os.Remove(pending)

	// hydra db reseed: the branch's stack is destroyed, the other kept.
	writeFile(seedRequest(root), nil)
	if err := markSeed(root, "refs/heads/feat", f.run); err != nil {
		t.Fatal(err)
	}
	if !exists(pending) || exists(seedRequest(root)) || f.ids() != "old:unavailable" {
		t.Errorf("reseed: pending %v, request %v, stacks %s", exists(pending), exists(seedRequest(root)), f.ids())
	}

	// A destroy that fails keeps the request, for the next start.
	os.Remove(pending)
	writeFile(seedRequest(root), nil)
	f = &fakeSupabase{stacks: stacks(), fail: "feat"}
	if err := markSeed(root, "refs/heads/feat", f.run); err == nil || !exists(seedRequest(root)) || exists(pending) {
		t.Errorf("failed destroy: %v, request %v, pending %v", err, exists(seedRequest(root)), exists(pending))
	}
}

func TestBriefErr(t *testing.T) {
	err := errors.New("failed to connect to `user=supabase_admin database=postgres`:\n" +
		"\t127.0.0.1:41322 (127.0.0.1): dial error: dial tcp 127.0.0.1:41322: connect: connection refused\n" +
		"\t127.0.0.1:41322 (127.0.0.1): dial error: dial tcp 127.0.0.1:41322: connect: connection refused")
	if got := briefErr(err); got != "connection refused" {
		t.Errorf("got %q", got)
	}
	if got := briefErr(errors.New("timeout: context deadline exceeded")); got != "timeout: context deadline exceeded" {
		t.Errorf("one line kept whole: %q", got)
	}
}

func TestSeedHelpers(t *testing.T) {
	if got := adminURL("postgresql://postgres:postgres@127.0.0.1:54322/postgres"); got != "postgresql://supabase_admin:postgres@127.0.0.1:54322/postgres" {
		t.Errorf("adminURL = %s", got)
	}
	for n, want := range map[int64]string{12: "12 B", 51_200: "50 kB", 95_840_000: "91.4 MB", 3 << 30: "3.0 GB"} {
		if got := size(n); got != want {
			t.Errorf("size(%d) = %s, want %s", n, got, want)
		}
	}
	// Supabase's own bookkeeping isn't the app's data.
	if !skipTable("auth.schema_migrations") || skipTable("auth.users") {
		t.Error("skipTable")
	}
}
