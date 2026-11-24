package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The file-system monitor must never hide a change: each kind of change is
// seen on the call right after it, with the monitor already running.
func TestWorkTreeSeesEveryChange(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("a/kept.txt", "1")
	write("a/edited.txt", "1")
	write("a/deleted.txt", "1")
	git("add", ".")
	git("commit", "-qm", "base")
	t.Cleanup(func() { exec.Command("git", "-C", dir, "fsmonitor--daemon", "stop").Run() })

	status := func() string {
		t.Helper()
		out, err := WorkTree(dir, "status", "--porcelain", "--untracked-files=all", "--no-renames").Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimRight(string(out), "\n")
	}
	if got := status(); got != "" { // starts the monitor where there is one
		t.Fatalf("clean tree reported %q", got)
	}
	write("a/edited.txt", "2")
	if got := status(); got != " M a/edited.txt" {
		t.Fatalf("after an edit: %q", got)
	}
	write("b/new.txt", "1")
	if got := status(); !strings.Contains(got, "?? b/new.txt") {
		t.Fatalf("after a new file: %q", got)
	}
	if err := os.Remove(filepath.Join(dir, "a/deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if got := status(); !strings.Contains(got, " D a/deleted.txt") {
		t.Fatalf("after a deletion: %q", got)
	}
}
