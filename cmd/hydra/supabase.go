package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/plan"
)

// prepareSupabase is hydra's part in running a worktree's own Supabase
// stack. The rendered process calls it three times, with the stack's
// environment:
//
//	hydra prepare supabase --before      before the stack file's start command
//	hydra prepare supabase --started N   after it; N is its exit status
//	hydra prepare supabase --stopping    before the stack file's stop command
//	hydra prepare supabase --stopped     after it
//
// and two more come from elsewhere:
//
//	hydra prepare supabase --ready       the step that what reads the database
//	                                     waits for: it ends once the database
//	                                     is started and migrated
//	hydra prepare supabase --migrate     the watcher's, when migration files
//	                                     change while the stack runs
//
// The Supabase CLI names a stack after the directory and the branch checked
// out in it. A worktree whose branch is renamed or switched therefore has a
// stack its own `supabase start` and `supabase stop` no longer find, still
// holding the slot's ports. hydra goes by the directory instead:
//
//   - before a start, it stops the stacks this directory has for other
//     branches (they keep their data until the worktree is removed);
//   - after a start, it applies the migrations the stack hasn't seen, through
//     the database's address rather than the CLI's idea of the current stack
//     (a new stack applies them all on its first start; a resumed one
//     doesn't), and puts back the files the CLI touches in the checkout;
//   - after a stop, it stops whatever stack of this directory still runs.
//
// It refuses to run without stack mode: without it, the CLI would reach the
// main checkout's database. And it refuses to run anywhere but in a
// worktree. A folder that was one and no longer is (it was removed with its
// stack up, and git couldn't delete all of it) is still the directory these
// commands run in, and both git and the Supabase CLI then answer for
// whatever lies above it: for a worktree kept inside the main checkout, the
// main checkout. `--stopping` is that check on its own: when it fails, the
// rendered stop leaves out the stack file's command, and the stack is
// stopped by its id (reap).
func prepareSupabase(args []string) error {
	fs := flag.NewFlagSet("prepare supabase", flag.ContinueOnError)
	before := fs.Bool("before", false, "")
	stopping := fs.Bool("stopping", false, "")
	stopped := fs.Bool("stopped", false, "")
	started := fs.Int("started", 0, "")
	ready := fs.Bool("ready", false, "")
	migrate := fs.Bool("migrate", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return fmt.Errorf("usage: hydra prepare supabase [--before | --started STATUS | --stopping | --stopped | --ready | --migrate] (run by the processes hydra renders)")
	}
	co, err := gitx.Find(".")
	if err != nil {
		return fmt.Errorf("prepare supabase: this directory is not a worktree (was it removed?), so nothing here is touched: %w", err)
	}
	if co.IsMain() {
		return fmt.Errorf("prepare supabase: this directory belongs to the main checkout, not to a worktree (was the worktree removed?); not touching the main checkout's database")
	}
	if *stopping {
		return nil
	}
	if *ready {
		return waitPrepared(co.Root, os.Getenv("HYDRA_SERVICE"), os.Getppid())
	}
	if os.Getenv("SUPABASE_EXPERIMENTAL_STACK") != "1" {
		return fmt.Errorf("prepare supabase: SUPABASE_EXPERIMENTAL_STACK isn't 1, so the Supabase CLI would reach the main checkout's database; not touching it")
	}
	branch := branchContext(co.Root)
	switch {
	case *before:
		// Whatever this start ends in, the database isn't ready until it does.
		// And it applies every migration itself: what the watcher couldn't
		// apply to the stack that ran before is this start's to say.
		os.Remove(supabaseMarker(co.Root))
		os.Remove(migrationsFailed(co.Root))
		n, err := retireSupabase(co.Root, branch, false)
		if n > 0 {
			fmt.Printf("hydra: stopped the Supabase stack this worktree had for another branch; the one for %s starts now (the other keeps its data until the worktree is removed)\n", strings.TrimPrefix(branch, "refs/heads/"))
		}
		return err
	case *stopped:
		os.Remove(supabaseMarker(co.Root))
		os.Remove(migrationsFailed(co.Root))
		_, err := retireSupabase(co.Root, "", false)
		return err
	}

	// After the start. Whatever it did, the checkout is put back first.
	_, terr := tidySupabase(co)
	if *started != 0 {
		return errExit(*started) // the start failed, and said why
	}
	var out bytes.Buffer
	cmd := exec.Command("supabase", "migration", "up", "--include-all", "--db-url", supabaseDBURL())
	cmd.Dir = co.Root
	cmd.Stdout, cmd.Stderr = io.MultiWriter(os.Stdout, &out), io.MultiWriter(os.Stderr, &out)
	err = cmd.Run()
	if err == nil {
		err = terr
	}
	if err != nil && strings.Contains(out.String(), "MigrationMissingLocalError") {
		return fmt.Errorf("this worktree's database has migrations that are no longer in supabase/migrations " +
			"(one was deleted or renamed after it ran). From the worktree's root, which reaches only this " +
			"worktree's stack: `supabase db reset` rebuilds it from the files (its data is lost), or " +
			"`supabase migration repair --status reverted <version>` forgets the missing ones")
	}
	if err != nil {
		return fmt.Errorf("supabase migration up: %w", err)
	}
	if *migrate {
		// The stack was started for the branch it was started for: a rename
		// since then is still worth saying (see supabaseBranchNote).
		return nil
	}
	return writeFile(supabaseMarker(co.Root), []byte(branch+"\n"))
}

// waitPrepared returns once the worktree's own database is started and
// migrated: the rendered Supabase process removes the marker before each
// start and writes it after a start that worked and applied every migration.
// It waits for as long as that takes. When the start fails, `hydra status`
// names the database as the problem; once that is fixed and it is started
// again, what waits here goes on without another command.
//
// parent is the process-compose that started this step: when it is gone,
// nothing is left to wait for.
func waitPrepared(root, name string, parent int) error {
	if name == "" {
		name = "supabase"
	}
	marker := supabaseMarker(root)
	began := time.Now()
	for said := false; ; time.Sleep(preparedPoll) {
		if _, err := os.Stat(marker); err == nil {
			if said {
				fmt.Printf("hydra: this worktree's database is ready (%s)\n", time.Since(began).Round(time.Second))
			}
			return nil
		}
		if orphaned(parent) {
			return fmt.Errorf("the process-compose that started this step is gone")
		}
		if !said {
			said = true
			fmt.Printf("hydra: waiting for %s to start this worktree's database and apply its migrations; what reads the database starts after that\n", name)
		}
	}
}

var preparedPoll = 300 * time.Millisecond

// supabaseDBURL is the address of this worktree's own database: the slot's
// port, with the Supabase CLI's local credentials.
func supabaseDBURL() string {
	port := os.Getenv("SUPABASE_DB_PORT")
	if port == "" {
		port = "54322"
	}
	return "postgresql://postgres:postgres@127.0.0.1:" + port + "/postgres"
}

// branchContext is what the Supabase CLI calls the branch of a checkout:
// the full ref HEAD points at, or "detached".
func branchContext(root string) string {
	ref, err := gitOut(root, "symbolic-ref", "-q", "HEAD")
	if err != nil || ref == "" {
		return "detached"
	}
	return ref
}

// supabaseMarker is there while a worktree's own Supabase stack is started
// and migrated: written after a start that worked, removed before the next
// one, after a stop, and when the worktree's stack starts. What reads the
// database waits for it (waitPrepared). It holds the branch the stack was
// started for, so `hydra status` can tell when the branch has since been
// renamed or switched.
func supabaseMarker(root string) string {
	return filepath.Join(root, ".process-compose", "supabase-branch")
}

// migrationsProblem is the problem to report when migrations didn't apply to
// a worktree's running database: the services here then run against a
// database that lacks what the branch's files describe.
func migrationsProblem(root, name string) *problem {
	data, err := os.ReadFile(migrationsFailed(root))
	if err != nil {
		return nil
	}
	text := name + " here: the migrations in supabase/migrations didn't all apply to this worktree's database. The Supabase CLI said:"
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if r := []rune(l); len(r) > 160 {
			l = string(r[:159]) + "…"
		}
		text += "\n    | " + l
	}
	return &problem{Blocks: true, Text: text,
		Fix: "correct the migration file: hydra applies it when the file changes   (to rebuild this worktree's database from the files, losing its data: supabase db reset, typed at this worktree's root)"}
}

// supabaseBranchNote is the problem to report when a worktree's Supabase
// stack runs for another branch than the one checked out now: the services
// keep using it (they go by its ports), but the CLI no longer finds it.
func supabaseBranchNote(root, name string) *problem {
	data, err := os.ReadFile(supabaseMarker(root))
	was := strings.TrimSpace(string(data))
	now := branchContext(root)
	if err != nil || was == "" || was == now {
		return nil
	}
	short := func(ref string) string { return strings.TrimPrefix(ref, "refs/heads/") }
	return &problem{
		Text: fmt.Sprintf("%s here was started for branch %s, and the branch is now %s. The services here keep using it, but `supabase` commands typed in this worktree won't find it.",
			name, short(was), short(now)),
		Fix: fmt.Sprintf("hydra process restart %s   (starts the database of %s, from its migrations and seeds)", name, short(now)),
	}
}

// cliIgnoreLine matches the lines stack mode appends to supabase/.gitignore
// on every start (.link.json, .supabase.lock.lease and their temp files).
var cliIgnoreLine = regexp.MustCompile(`^\.{1,2}(link\.json|supabase\.lock)`)

// supabaseExclude goes into the repository's info/exclude, so the files the
// CLI writes in a worktree never show up as changes there.
var supabaseExclude = []string{
	"**/supabase/supabase.lock",
	"**/supabase/.link.json",
	"**/supabase/..link.json.*.tmp",
	"**/supabase/.supabase.lock.*",
}

const excludeHeader = "# hydra: files the Supabase CLI writes for a worktree's own stack"

// tidySupabase undoes the CLI's edit to the tracked supabase/.gitignore when
// that edit is all that changed, and ignores the files it writes through
// info/exclude instead. Agents in the worktree then see no change they
// didn't make. It reports whether it put the file back.
//
// The main checkout gets the same, though hydra doesn't wrap the start of
// its Supabase (the stack file's own command does it, by --stack-id): a
// stack-mode start appends there too, and a main checkout with changes
// stops `hydra update`. Main's hydra-router tidies when the file changes
// (gitignoreWatch), and `hydra update` before it checks that main is clean.
func tidySupabase(co gitx.Checkout) (bool, error) {
	path := filepath.Join(co.Root, "supabase", ".gitignore")
	index, ok, err := cliEdited(co.Root)
	if err != nil {
		return false, err
	}
	if ok {
		if err := os.WriteFile(path, index, 0o644); err != nil {
			return false, err
		}
	}
	return ok, ensureExclude(co.CommonDir)
}

// cliEdited reports whether the CLI's lines are all that changed in the
// checkout's supabase/.gitignore, and returns the file as git has it.
func cliEdited(root string) ([]byte, bool, error) {
	work, err := os.ReadFile(filepath.Join(root, "supabase", ".gitignore"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	index, err := exec.Command("git", "-C", root, "show", ":supabase/.gitignore").Output()
	if err != nil {
		return nil, false, nil // not tracked: nobody sees a change
	}
	rest, ok := bytes.CutPrefix(work, index)
	return index, ok && len(rest) > 0 && onlyCLILines(rest), nil
}

// gitignoreWatch is main's hydra-router looking at supabase/.gitignore: when
// the file changes (a start of main's Supabase appended to it), it is tidied.
// A stat every two seconds; git only when the file changed.
type gitignoreWatch struct {
	co   gitx.Checkout
	last time.Time
}

func (g *gitignoreWatch) look() {
	info, err := os.Stat(filepath.Join(g.co.Root, "supabase", ".gitignore"))
	if err != nil || info.ModTime().Equal(g.last) {
		return
	}
	g.last = info.ModTime()
	restored, err := tidySupabase(g.co)
	switch {
	case err != nil:
		logf("can't put supabase/.gitignore back after the Supabase CLI's edit: %v", err)
	case restored:
		logf("put supabase/.gitignore back: the Supabase CLI had appended its lock files to it (they are ignored through .git/info/exclude)")
		if info, err := os.Stat(filepath.Join(g.co.Root, "supabase", ".gitignore")); err == nil {
			g.last = info.ModTime()
		}
	}
}

func onlyCLILines(b []byte) bool {
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" && !cliIgnoreLine.MatchString(line) {
			return false
		}
	}
	return true
}

func ensureExclude(commonDir string) error {
	path := filepath.Join(commonDir, "info", "exclude")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	have := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		have[strings.TrimSpace(line)] = true
	}
	var add []string
	for _, p := range supabaseExclude {
		if !have[p] {
			add = append(add, p)
		}
	}
	if len(add) == 0 {
		return nil
	}
	var b strings.Builder
	b.Write(data)
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		b.WriteString("\n")
	}
	if !have[excludeHeader] {
		b.WriteString(excludeHeader + "\n")
	}
	for _, p := range add {
		b.WriteString(p + "\n")
	}
	return writeFile(path, []byte(b.String()))
}

const (
	envBegin = "# >>> hydra: this worktree's own Supabase stack (rewritten by hydra; edit outside this block)"
	envEnd   = "# <<< hydra"
)

// syncEnvLocal keeps a block in the worktree's root .env.local while the
// worktree runs its own Supabase: the CLI reads that file from the directory
// it runs in, so `supabase migration up` or `supabase db reset` typed at the
// worktree's root reach this worktree's stack, not the main checkout's
// database. Removed again when the worktree goes back to the baseline's.
// Commands run from a subdirectory don't read it.
func syncEnvLocal(t *target, p *plan.Plan) error {
	var block []string
	for _, row := range p.Services {
		src := t.stack.Services[row.Source]
		if row.Local && src != nil && src.Kind == config.KindSupabase {
			block = t.stack.SupabaseEnv(src, p.Slot)
			break
		}
	}
	path := filepath.Join(t.checkout.Root, ".env.local")
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		// Often a link to the main checkout's file: writing through it would
		// point the main checkout's CLI at this worktree's stack.
		if len(block) > 0 {
			fmt.Fprintf(os.Stderr, "hydra: %s is a symlink, so hydra leaves it alone; Supabase CLI commands typed in this worktree reach the main checkout's database, not this worktree's\n", path)
		}
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	kept := withoutBlock(string(data))
	next := kept
	if len(block) > 0 {
		if next != "" && !strings.HasSuffix(next, "\n") {
			next += "\n"
		}
		next += envBegin + "\n" + strings.Join(block, "\n") + "\n" + envEnd + "\n"
	}
	if next == string(data) {
		return nil
	}
	if next == "" {
		return os.Remove(path) // hydra created it; nothing else is in it
	}
	if len(block) > 0 {
		if out, err := exec.Command("git", "-C", t.checkout.Root, "check-ignore", "-q", ".env.local").CombinedOutput(); err != nil {
			return fmt.Errorf("%s/.env.local isn't git-ignored, so hydra won't write to it (%s)", t.checkout.Root, strings.TrimSpace(string(out)))
		}
	}
	return os.WriteFile(path, []byte(next), 0o644)
}

func withoutBlock(s string) string {
	start := strings.Index(s, envBegin)
	if start < 0 {
		return s
	}
	end := strings.Index(s[start:], envEnd)
	if end < 0 {
		return s[:start]
	}
	end += start + len(envEnd)
	if end < len(s) && s[end] == '\n' {
		end++
	}
	return s[:start] + s[end:]
}
