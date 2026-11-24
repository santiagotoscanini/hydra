package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/slots"
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

// slotted adds a worktree to the repository and gives it a slot, the way a
// `hydra up` there does. Sockets and locks go to a directory of the test's.
func slotted(t *testing.T, co gitx.Checkout, name string) slots.Entry {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if out, err := exec.Command("git", "-C", co.Main, "worktree", "add", "-q", "-b", name, root).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	wt, err := gitx.Find(root)
	if err != nil {
		t.Fatal(err)
	}
	slot, _, err := slots.Acquire(co.CommonDir, wt.Root, 9, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return slots.Entry{Root: wt.Root, Slot: slot}
}

// stackWrote puts into a folder what a running stack leaves there: hydra's
// own files, and with more, a build's output.
func stackWrote(t *testing.T, root string, more bool) {
	t.Helper()
	files := map[string]string{
		".process-compose/hydra.yaml":   "processes: {}\n",
		".process-compose/logs/api.log": "listening\n",
		".env.local":                    envBegin + "\nSUPABASE_API_PORT=54521\n" + envEnd + "\n",
		"supabase/.temp/tmp/.keep":      "",
	}
	if more {
		files["apps/web/.next/trace"] = "{}\n"
	}
	for name, body := range files {
		if err := writeFile(filepath.Join(root, name), []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	os.Remove(filepath.Join(root, "supabase/.temp/tmp/.keep")) // the directory is what hydra makes
}

func removeWorktree(t *testing.T, co gitx.Checkout, root string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", co.Main, "worktree", "remove", "--force", root).CombinedOutput(); err != nil {
		t.Fatalf("git worktree remove: %v\n%s", err, out)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// A worktree removed with its stack up: git deletes everything, and the
// stack, still running, writes its files again. Once it is stopped they are
// all that is in the folder, and they are hydra's to delete.
func TestWhatHydraLeftInARemovedWorktreeGoesWithIt(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	co := gitRepo(t)
	e := slotted(t, co, "one")
	removeWorktree(t, co, e.Root)
	stackWrote(t, e.Root, false)

	if gone, _ := slots.Gone(co.CommonDir); len(gone) != 1 {
		t.Fatalf("a folder without `.git` still counts as a worktree: Gone = %v", gone)
	}
	r, err := reap(co, e, reaping{})
	if err != nil || !r.freed || r.left {
		t.Fatalf("reap = %+v, %v", r, err)
	}
	if strings.Join(r.did, ", ") != "deleted what was left of its folder" {
		t.Errorf("did = %v", r.did)
	}
	if exists(e.Root) {
		t.Errorf("%s is still there", e.Root)
	}
	if holdsSlot(co.CommonDir, e.Root) {
		t.Error("the slot is still taken")
	}
	if left, _ := filepath.Glob(filepath.Join(os.TempDir(), "hydra-*")); len(left) != 0 {
		t.Errorf("left in the temporary directory: %v", left)
	}

	// Nothing left at all: nothing to say, and the slot is freed the same.
	e = slotted(t, co, "two")
	removeWorktree(t, co, e.Root)
	if r, err := reap(co, e, reaping{}); err != nil || !r.freed || r.left || len(r.did) != 0 {
		t.Errorf("reap of a worktree git deleted whole = %+v, %v", r, err)
	}
}

// What a service wrote (a build's output, a cache) is not hydra's. Only the
// worktree's own watcher, which saw the removal happen under a running
// stack, knows that it is what git gave up on; whoever finds the folder
// later leaves it and says so.
func TestOtherFilesInARemovedWorktreeGoOnlyWhenTheWatcherSawItRemoved(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	co := gitRepo(t)

	e := slotted(t, co, "later")
	removeWorktree(t, co, e.Root)
	stackWrote(t, e.Root, true)
	r, err := reap(co, e, reaping{})
	if err != nil || !r.freed || !r.left || len(r.did) != 0 {
		t.Fatalf("reap = %+v, %v; want the slot freed and the folder reported", r, err)
	}
	if !exists(filepath.Join(e.Root, "apps/web/.next/trace")) {
		t.Error("a file that isn't hydra's was deleted")
	}
	for _, name := range []string{".process-compose", ".env.local", "supabase"} {
		if exists(filepath.Join(e.Root, name)) {
			t.Errorf("%s is hydra's, and still there", name)
		}
	}

	e = slotted(t, co, "seen")
	removeWorktree(t, co, e.Root)
	stackWrote(t, e.Root, true)
	r, err = reap(co, e, reaping{saw: true})
	if err != nil || !r.freed || r.left || exists(e.Root) {
		t.Fatalf("the watcher's reap = %+v, %v (folder still there: %v)", r, err, exists(e.Root))
	}
}

// git gives up at the first directory it can't empty, and forgets the
// worktree all the same: most of the checkout is still there, with a `.git`
// that names a git directory that is gone.
func TestAWorktreeGitGaveUpOnHalfwayIsRemoved(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	co := gitRepo(t)
	half := func(name string) slots.Entry {
		e := slotted(t, co, name)
		stackWrote(t, e.Root, true)
		// What `git worktree remove` does last, whatever became of the files.
		if err := os.RemoveAll(filepath.Join(co.CommonDir, "worktrees", name)); err != nil {
			t.Fatal(err)
		}
		return e
	}

	e := half("later")
	if !e.Missing() || presenceOf(co.Main, e.Root) != removed {
		t.Fatalf("Missing = %v, presence = %v", e.Missing(), presenceOf(co.Main, e.Root))
	}
	r, err := reap(co, e, reaping{})
	if err != nil || !r.freed || !r.left {
		t.Fatalf("reap = %+v, %v", r, err)
	}
	if !exists(filepath.Join(e.Root, "supabase", ".gitignore")) || exists(filepath.Join(e.Root, ".process-compose")) {
		t.Error("want the checkout's files kept and hydra's deleted")
	}

	e = half("seen")
	if r, err := reap(co, e, reaping{saw: true}); err != nil || !r.freed || r.left || exists(e.Root) {
		t.Errorf("the watcher's reap = %+v, %v (folder still there: %v)", r, err, exists(e.Root))
	}
}

// A `.git` deleted by hand looks the same from inside the folder, but git
// still lists the worktree, and `git worktree repair` mends it. Nothing of
// it is deleted, and it keeps its slot unless someone asks for it.
func TestAWorktreeGitStillListsKeepsItsFilesAndItsSlot(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	co := gitRepo(t)
	e := slotted(t, co, "broken")
	stackWrote(t, e.Root, true)
	if err := os.Remove(filepath.Join(e.Root, ".git")); err != nil {
		t.Fatal(err)
	}
	if presenceOf(co.Main, e.Root) != leaving {
		t.Fatalf("presence = %v, want leaving", presenceOf(co.Main, e.Root))
	}
	for _, how := range []reaping{{}, {saw: true}} {
		r, err := reap(co, e, how)
		if err != nil || r.freed || r.left || len(r.did) != 0 {
			t.Fatalf("reap(%+v) = %+v, %v; want nothing done", how, r, err)
		}
	}
	if !holdsSlot(co.CommonDir, e.Root) || !exists(filepath.Join(e.Root, ".process-compose", "hydra.yaml")) || !exists(filepath.Join(e.Root, "apps/web/.next/trace")) {
		t.Fatal("something of a worktree git still lists was taken away")
	}

	// `hydra slots free`: the slot goes, the files stay.
	r, err := reap(co, e, reaping{anyway: true})
	if err != nil || !r.freed || r.left {
		t.Fatalf("reap, asked for = %+v, %v", r, err)
	}
	if !exists(filepath.Join(e.Root, ".process-compose", "hydra.yaml")) || !exists(filepath.Join(e.Root, "supabase", ".gitignore")) {
		t.Error("freeing the slot of a worktree git still lists deleted files")
	}
}

// A worktree that is still there only loses its slot.
func TestFreeingTheSlotOfAWorktreeThatIsThereDeletesNothing(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	co := gitRepo(t)
	e := slotted(t, co, "here")
	stackWrote(t, e.Root, true)
	r, err := reap(co, e, reaping{anyway: true})
	if err != nil || !r.freed || r.left || len(r.did) != 0 {
		t.Fatalf("reap = %+v, %v", r, err)
	}
	for _, name := range []string{".git", ".process-compose/hydra.yaml", ".env.local", "apps/web/.next/trace"} {
		if !exists(filepath.Join(e.Root, name)) {
			t.Errorf("%s was deleted", name)
		}
	}
}

// The watcher's reap and a `hydra up` somewhere else may come for the same
// worktree: one does the work, the other leaves it alone.
func TestOnlyOneCleansUpAfterAWorktreeAtATime(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	co := gitRepo(t)
	e := slotted(t, co, "one")
	removeWorktree(t, co, e.Root)
	stackWrote(t, e.Root, false)

	unlock, mine := lockReap(e.Root, false)
	if !mine {
		t.Fatal("the lock wasn't free")
	}
	if _, again := lockReap(e.Root, false); again {
		t.Fatal("two hold the lock")
	}
	if r, err := reap(co, e, reaping{}); err != nil || r.freed || len(r.did) != 0 || !exists(e.Root) {
		t.Errorf("reap while another holds the lock = %+v, %v; want nothing done", r, err)
	}
	unlock(false)
	if r, err := reap(co, e, reaping{}); err != nil || !r.freed || exists(e.Root) {
		t.Errorf("reap after the lock was given back = %+v, %v", r, err)
	}
	// Whoever waited for the lock finds the job done.
	if r, err := reap(co, e, reaping{anyway: true}); err != nil || !r.freed || len(r.did) != 0 {
		t.Errorf("reap of a slot that is already free = %+v, %v", r, err)
	}
}

// Never the folder the main checkout is in, whatever the registry says.
func TestTheMainCheckoutIsNeverInsideWhatIsDeleted(t *testing.T) {
	for _, c := range []struct {
		path, dir string
		want      bool
	}{
		{"/repo", "/repo", true},
		{"/repo", "/", true},
		{"/home/me/repo", "/home/me", true},
		{"/repo", "/repo/.claude/worktrees/x", false},
		{"/repo", "/repo-2", false},
		{"/repo", "/wt/x", false},
	} {
		if got := inside(c.path, c.dir); got != c.want {
			t.Errorf("inside(%q, %q) = %v, want %v", c.path, c.dir, got, c.want)
		}
	}
	co := gitRepo(t)
	if cleared, left := clearFolder(co.Main, filepath.Dir(co.Main), true); cleared || !left || !exists(filepath.Join(co.Main, ".git")) {
		t.Errorf("clearFolder on a folder that holds the main checkout: cleared=%v left=%v", cleared, left)
	}
}

// The steps around a worktree's own database only ever run in a worktree. A
// removed one can leave a folder behind inside the main checkout, where git
// and the Supabase CLI answer for the main checkout.
func TestPrepareSupabaseRunsOnlyInAWorktree(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("SUPABASE_EXPERIMENTAL_STACK", "1")
	co := gitRepo(t)
	e := slotted(t, co, "one")

	t.Chdir(e.Root)
	if err := prepareSupabase([]string{"--stopping"}); err != nil {
		t.Errorf("in a worktree: %v", err)
	}

	left := filepath.Join(co.Main, ".claude", "worktrees", "gone")
	os.MkdirAll(left, 0o755)
	t.Chdir(left)
	for _, flag := range []string{"--stopping", "--before", "--stopped", "--ready", "--migrate"} {
		if err := prepareSupabase([]string{flag}); err == nil || !strings.Contains(err.Error(), "main checkout") {
			t.Errorf("%s in a folder of the main checkout: %v", flag, err)
		}
	}

	t.Chdir(t.TempDir())
	if err := prepareSupabase([]string{"--stopping"}); err == nil || !strings.Contains(err.Error(), "not a worktree") {
		t.Errorf("outside any repository: %v", err)
	}
}

// A worktree kept inside the main checkout and removed under its stack can
// leave a folder there without its `.git`. git then takes the folder for a
// part of the main checkout, whose plan is the whole stack on main's ports.
// The watcher that still runs there must not go by that.
func TestTheWatcherDoesNotPlanForAFolderGitTakesForMain(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	co := gitRepo(t)
	file := filepath.Join(co.Main, "process-compose.yaml")
	if err := os.WriteFile(file, []byte(checkStack), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(co.Main, ".wt", "inner")
	if out, err := exec.Command("git", "-C", co.Main, "worktree", "add", "-q", "-b", "inner", root).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	tgt, err := resolve(root, file)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.checkout.IsMain() {
		t.Fatal("the worktree resolved to the main checkout")
	}
	out := filepath.Join(tgt.checkout.Root, ".process-compose", "hydra.yaml")

	// git gave up on the folder and forgot the worktree.
	os.Remove(filepath.Join(root, ".git"))
	os.RemoveAll(filepath.Join(co.CommonDir, "worktrees", "inner"))
	if again, err := resolve(root, file); err != nil || !again.checkout.IsMain() {
		t.Fatalf("expected git to take the folder for main: %+v, %v", again, err)
	}

	_, err = step(tgt, options{slot: -1}, "true", out, socketFor(tgt.checkout.Root))
	if err == nil || !strings.Contains(err.Error(), "no longer a worktree") {
		t.Errorf("step = %v, want it to refuse", err)
	}
	if exists(out) {
		t.Errorf("the watcher rendered a file for it: %s", out)
	}
	if tgt.checkout.IsMain() {
		t.Error("the watcher's checkout became the main checkout")
	}
}
