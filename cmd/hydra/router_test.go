package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The file can be ahead of what main runs until `hydra update`. Raising
// max_slots moves the dispatch ports, but main's web was started with the
// old ones: a router that followed the file would leave them, and hand
// them to the worktree that gets the next slot.
func TestTheRouterListensWhereMainsProcessesWereToldTo(t *testing.T) {
	dir := t.TempDir()
	file, startedFrom := filepath.Join(dir, "process-compose.yaml"), filepath.Join(dir, "started.yaml")
	if err := os.WriteFile(startedFrom, []byte(checkStack), 0o644); err != nil {
		t.Fatal(err)
	}
	raised := strings.Replace(checkStack, "max_slots: 5", "max_slots: 9", 1)
	if raised == checkStack {
		t.Fatal("the test stack sets no max_slots to raise")
	}
	if err := os.WriteFile(file, []byte(raised), 0o644); err != nil {
		t.Fatal(err)
	}
	want := checkSt(t).DispatchPort(checkSt(t).Services["api"])

	st, err := routerState(t.TempDir(), file, startedFrom)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Services["api"].Dispatch; got != want {
		t.Errorf("dispatch for api on :%d, but main's web calls :%d", got, want)
	}

	// Main started by a hydra that kept no copy: the file is all there is.
	st, err = routerState(t.TempDir(), file, filepath.Join(dir, "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Services["api"].Dispatch; got == want {
		t.Errorf("without a copy, the router didn't read the file (dispatch still :%d)", got)
	}
}
