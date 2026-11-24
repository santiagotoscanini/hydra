package main

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// names lists the processes of a rendered file whose command is want.
func withCommand(t *testing.T, file []byte, want string) []string {
	t.Helper()
	var doc struct {
		Processes map[string]struct {
			Command string `yaml:"command"`
		} `yaml:"processes"`
	}
	if err := yaml.Unmarshal(file, &doc); err != nil {
		t.Fatal(err)
	}
	var out []string
	for name, p := range doc.Processes {
		if p.Command == want {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

const runsNow = `
processes:
  hydra-watch: { command: old }
  supabase: { command: old, namespace: main }
  temporal: { command: same, namespace: main }
  api: { command: old, depends_on: { supabase: { condition: process_healthy } } }
  gone: { command: old }
`

// A migration appeared: the database, the step that waits for it, the API
// (which ran here already), a worker with its namespace step, and an agent
// that waits for both.
const runsNext = `
processes:
  hydra-watch: { command: new }
  temporal: { command: same, namespace: main }
  colima: { command: new, namespace: main }
  supabase: { command: new, depends_on: { colima: { condition: process_completed_successfully } } }
  hydra-ready-supabase: { command: new }
  hydra-prepare-worker: { command: new, depends_on: { temporal: { condition: process_healthy } } }
  api: { command: new, depends_on: { hydra-ready-supabase: { condition: process_completed_successfully } } }
  worker:
    command: new
    depends_on:
      hydra-ready-supabase: { condition: process_completed_successfully }
      hydra-prepare-worker: { condition: process_completed_successfully }
  agent:
    command: new
    depends_on:
      api: { condition: process_healthy }
      worker: { condition: process_log_ready }
`

// process-compose starts what an update adds in no order of its own, and a
// process whose dependency isn't in the project yet doesn't wait for it. So
// each step only holds processes whose dependencies an earlier step put
// there.
func TestAnUpdateIsHandedOverInDependencyOrder(t *testing.T) {
	steps := updateSteps([]byte(runsNow), []byte(runsNext))
	if len(steps) != 3 {
		t.Fatalf("%d steps, want 3", len(steps))
	}
	want := [][]string{
		// What waits for nothing that changed. The API waits for the step,
		// so it keeps running as it was. Nothing is dropped yet.
		{"colima", "hydra-prepare-worker", "hydra-ready-supabase"},
		{"api", "colima", "hydra-prepare-worker", "hydra-ready-supabase", "supabase", "worker"},
	}
	for i, names := range want {
		if got := withCommand(t, steps[i], "new"); strings.Join(got, " ") != strings.Join(names, " ") {
			t.Errorf("step %d changes %v, want %v", i+1, got, names)
		}
		if got := withCommand(t, steps[i], "old"); !contains(got, "gone") || !contains(got, "hydra-watch") {
			t.Errorf("step %d dropped a process or restarted the watcher early: old = %v", i+1, got)
		}
	}
	if got := withCommand(t, steps[0], "old"); !contains(got, "api") || !contains(got, "supabase") {
		t.Errorf("step 1 should leave api and supabase as they run: %v", got)
	}
	// The last one is the file itself: the agent, the watcher, and "gone" gone.
	if string(steps[2]) != runsNext {
		t.Errorf("the last step isn't the new file:\n%s", steps[2])
	}
}

func TestAnUpdateWithNothingToOrderIsOneStep(t *testing.T) {
	next := strings.Replace(runsNow, "api: { command: old", "api: { command: new", 1)
	if steps := updateSteps([]byte(runsNow), []byte(next)); len(steps) != 1 || string(steps[0]) != next {
		t.Errorf("steps = %q", steps)
	}
	// Nothing runs yet, or the file on disk can't be read: the new file, whole.
	for _, current := range []string{"", "processes: {}", ":\n  - not yaml"} {
		if steps := updateSteps([]byte(current), []byte(runsNext)); len(steps) != 1 || string(steps[0]) != runsNext {
			t.Errorf("from %q: %d steps", current, len(steps))
		}
	}
}

// A step that process-compose refuses leaves the file at the last step that
// went through, which is what runs: the next try starts from there.
func TestAFailedStepLeavesTheFileAtWhatRuns(t *testing.T) {
	out := filepath.Join(t.TempDir(), "hydra.yaml")
	os.WriteFile(out, []byte(runsNow), 0o644)
	var sent [][]byte
	err := applyProject(out, []byte(runsNow), []byte(runsNext), func(file string) error {
		data, _ := os.ReadFile(file)
		sent = append(sent, data)
		if len(sent) == 2 {
			return errors.New("refused")
		}
		return nil
	})
	if err == nil || len(sent) != 2 {
		t.Fatalf("err = %v after %d steps", err, len(sent))
	}
	if left, _ := os.ReadFile(out); string(left) != string(sent[0]) {
		t.Errorf("the file was left at:\n%s", left)
	}
	// The next try goes on from there: what the first step did isn't done again.
	steps := updateSteps(sent[0], []byte(runsNext))
	if len(steps) != 2 {
		t.Fatalf("%d steps left, want 2", len(steps))
	}
	if got := withCommand(t, steps[0], "new"); !contains(got, "supabase") || !contains(got, "worker") || contains(got, "agent") {
		t.Errorf("the step after the failed one changes %v", got)
	}

	// All of them, when nothing fails; the file ends as the new one.
	sent = nil
	os.WriteFile(out, []byte(runsNow), 0o644)
	if err := applyProject(out, []byte(runsNow), []byte(runsNext), func(file string) error {
		data, _ := os.ReadFile(file)
		sent = append(sent, data)
		return nil
	}); err != nil || len(sent) != 3 {
		t.Fatalf("err = %v, %d steps", err, len(sent))
	}
	if left, _ := os.ReadFile(out); string(left) != runsNext {
		t.Errorf("the file ended as:\n%s", left)
	}
}
