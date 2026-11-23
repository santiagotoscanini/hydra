package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santiagotoscanini/hydra/internal/config"
)

// fakeSupabase answers like the Supabase CLI's stack commands.
type fakeSupabase struct {
	stacks []supabaseStack
	calls  []string
	fail   string // a stack id whose destroy fails
}

func (f *fakeSupabase) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	id := ""
	for i, a := range args {
		if a == "--stack-id" && i+1 < len(args) {
			id = args[i+1]
		}
	}
	switch {
	case args[0] == "stack" && args[1] == "list":
		var rows []string
		for _, s := range f.stacks {
			rows = append(rows, fmt.Sprintf(`{"id":%q,"project_root":%q,"name":"default","branch_context":%q,"runtime":"docker","owner":%q}`, s.ID, s.Root, s.Branch, s.Owner))
		}
		return []byte(`{"stacks":[` + strings.Join(rows, ",") + `],"message":""}`), nil
	case args[0] == "stack" && args[1] == "destroy":
		if id == f.fail {
			return nil, fmt.Errorf("supabase stack destroy: Docker isn't reachable")
		}
		var kept []supabaseStack
		for _, s := range f.stacks {
			if s.ID != id {
				kept = append(kept, s)
			}
		}
		f.stacks = kept
	case args[0] == "stop":
		for i := range f.stacks {
			if f.stacks[i].ID == id {
				f.stacks[i].Owner = "unavailable"
			}
		}
	}
	return nil, nil
}

func (f *fakeSupabase) ids() string {
	var out []string
	for _, s := range f.stacks {
		out = append(out, s.ID+":"+s.Owner)
	}
	return strings.Join(out, " ")
}

// The Supabase CLI keeps a stack per directory and branch. hydra goes by the
// directory: every stack of the worktree, whatever branch name it was made
// under, and nobody else's.
func TestRetireSupabaseGoesByTheDirectory(t *testing.T) {
	stacks := func() []supabaseStack {
		return []supabaseStack{
			{ID: "old", Root: "/wt/a", Branch: "refs/heads/t3code/tmp", Owner: "unavailable"},
			{ID: "now", Root: "/wt/a", Branch: "refs/heads/feature", Owner: "reachable"},
			{ID: "other", Root: "/wt/b", Branch: "refs/heads/feature", Owner: "reachable"},
			{ID: "prefix", Root: "/wt/a-2", Branch: "refs/heads/x", Owner: "reachable"},
		}
	}

	// The worktree is gone: all of its stacks are destroyed, with their data.
	f := &fakeSupabase{stacks: stacks()}
	if n, err := retireSupabaseWith(f.run, "/wt/a", "", true); err != nil || n != 2 {
		t.Fatalf("destroyed %d, %v", n, err)
	}
	if got := f.ids(); got != "other:reachable prefix:reachable" {
		t.Errorf("left after the destroy: %s", got)
	}

	// Before a start: the stack of another branch is stopped (it holds the
	// slot's ports) and keeps its data; the current branch's is left alone.
	f = &fakeSupabase{stacks: stacks()}
	f.stacks[0].Owner = "reachable"
	if n, err := retireSupabaseWith(f.run, "/wt/a", "refs/heads/feature", false); err != nil || n != 1 {
		t.Fatalf("stopped %d, %v", n, err)
	}
	if got := f.ids(); got != "old:unavailable now:reachable other:reachable prefix:reachable" {
		t.Errorf("after the stop: %s", got)
	}
	if strings.Contains(strings.Join(f.calls, "\n"), "destroy") {
		t.Errorf("a stop destroyed something: %v", f.calls)
	}

	// After a stop: whatever of this directory still runs is stopped, also
	// the stack the CLI's own `supabase stop` no longer finds.
	f = &fakeSupabase{stacks: stacks()}
	if n, err := retireSupabaseWith(f.run, "/wt/a", "", false); err != nil || n != 1 {
		t.Fatalf("stopped %d, %v", n, err)
	}
	if got := f.ids(); got != "old:unavailable now:unavailable other:reachable prefix:reachable" {
		t.Errorf("after the stop: %s", got)
	}

	// A destroy that fails is reported, so the slot stays taken and the next
	// start tries again.
	f = &fakeSupabase{stacks: stacks(), fail: "now"}
	if n, err := retireSupabaseWith(f.run, "/wt/a", "", true); err == nil || n != 1 {
		t.Errorf("destroyed %d, err %v; want the failure reported", n, err)
	}
}

func TestPortTakenOnlyWhenTheOutputSaysSo(t *testing.T) {
	for _, line := range []string{
		"listen tcp 127.0.0.1:21110: bind: address already in use",
		"Error: listen EADDRINUSE: address already in use :::3000",
		"Port 60527 for x:http at 0.0.0.0:60527 is in use by another process; free it",
		"Port 3000 is already in use",
	} {
		if !portInUse.MatchString(line) {
			t.Errorf("not recognised as a taken port: %s", line)
		}
	}
	for _, line := range []string{"panic: nil pointer", "connection refused", "exit status 1"} {
		if portInUse.MatchString(line) {
			t.Errorf("taken for a port problem: %s", line)
		}
	}
	// A service that died of something else isn't looked up at all.
	st := &config.Stack{Services: map[string]*config.Service{"api": {Port: 1}}}
	st.Hydra.SlotStride = 100
	if h := portTaken(st, "api", 1, []string{"panic: nil pointer"}); h != nil {
		t.Errorf("portTaken = %+v for a crash that isn't about ports", h)
	}
}

func TestTakenWording(t *testing.T) {
	left := &holder{Port: 8480, Pid: 41, Cmd: "api", Dir: "/wt/a/services/core"}
	if got := takenFix(left, "/wt/a", "hydra process restart api"); got != "kill 41 && hydra process restart api" {
		t.Errorf("fix for a leftover = %q", got)
	}
	if !strings.Contains(takenText(left, "/wt/a"), "left over from an earlier run of this stack") {
		t.Errorf("text for a leftover = %q", takenText(left, "/wt/a"))
	}
	// Someone else's program is never offered a kill.
	other := &holder{Port: 8480, Pid: 42, Cmd: "Slack", Dir: "/"}
	if got := takenFix(other, "/wt/a", "hydra process restart api"); strings.Contains(got, "kill") {
		t.Errorf("fix for another program = %q", got)
	}
	// A sibling directory with the same prefix isn't this checkout.
	sibling := &holder{Port: 8480, Pid: 43, Cmd: "api", Dir: "/wt/a-2/services"}
	if strings.Contains(takenFix(sibling, "/wt/a", "x"), "kill") {
		t.Error("a sibling worktree's program was taken for a leftover")
	}
}

func TestUpArguments(t *testing.T) {
	for _, args := range [][]string{{"-D"}, {"--detached"}, {"-p", "8099", "-t=false"}} {
		if !detached(args) {
			t.Errorf("%v should count as detached", args)
		}
	}
	if detached([]string{"-p", "8099", "web"}) {
		t.Error("a plain up counted as detached")
	}
	got := strings.Join(connFlags([]string{"-D", "-p", "8099", "--address=10.0.0.1", "-L", "x.log", "web"}), " ")
	if got != "-p 8099 --address=10.0.0.1" {
		t.Errorf("connFlags = %q", got)
	}
}

func TestSocketForIsStableAcrossRemoval(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wt")
	os.Mkdir(dir, 0o755)
	before := socketFor(dir)
	os.RemoveAll(dir)
	if after := socketFor(dir); after != before {
		t.Errorf("the socket of a removed worktree can't be found: %s, then %s", before, after)
	}
}

// `hydra up web` in a worktree means "run web here as well": the name goes
// into the rendered file, and process-compose is not handed it (it would
// start web alone). A flag's value that equals a name is not a name.
func TestDropNamesGoesByPosition(t *testing.T) {
	a := parsePC([]string{"up", "-D", "-L", "web", "web", "db", "--tui=false"})
	if strings.Join(a.names, ",") != "web,db" {
		t.Fatalf("names = %v", a.names)
	}
	a.dropNames(func(n string) bool { return n != "db" }) // db is shared: not here
	if got := strings.Join(a.rest, " "); got != "-D -L web web --tui=false" {
		t.Errorf("after dropping db: %q", got)
	}
	a.dropNames(func(string) bool { return false })
	if got := strings.Join(a.rest, " "); got != "-D -L web --tui=false" {
		t.Errorf("after dropping every name: %q (the log file's name must stay)", got)
	}
}

// A command line that already says where process-compose is isn't second-guessed.
func TestConnFlagsOnTheCommandLineWin(t *testing.T) {
	if got := connFlags([]string{"process", "list", "-p", "8099"}); strings.Join(got, " ") != "-p 8099" {
		t.Errorf("connFlags = %v", got)
	}
	if got := connFlags([]string{"process", "list"}); len(got) != 0 {
		t.Errorf("connFlags = %v, want none", got)
	}
}
