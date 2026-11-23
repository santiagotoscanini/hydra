package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/plan"
)

// prepareSupabase runs after a worktree's Supabase stack starts, with the
// stack's environment. It applies migrations the stack hasn't seen (a new
// stack applies them all on its first start; a resumed one doesn't), then
// puts back the files the CLI touches in the checkout.
//
// It refuses to run without stack mode: without it, the CLI would apply this
// branch's migrations to the main checkout's database.
func prepareSupabase(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: hydra prepare supabase (run by the rendered supabase process)")
	}
	if os.Getenv("SUPABASE_EXPERIMENTAL_STACK") != "1" {
		return fmt.Errorf("prepare supabase: SUPABASE_EXPERIMENTAL_STACK isn't 1, so the Supabase CLI would reach the main checkout's database; not applying migrations")
	}
	co, err := gitx.Find(".")
	if err != nil {
		return err
	}
	var out bytes.Buffer
	cmd := exec.Command("supabase", "migration", "up", "--include-all")
	cmd.Dir = co.Root
	cmd.Stdout, cmd.Stderr = io.MultiWriter(os.Stdout, &out), io.MultiWriter(os.Stderr, &out)
	err = cmd.Run()
	if terr := tidySupabase(co); terr != nil && err == nil {
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
	return nil
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
// didn't make.
func tidySupabase(co gitx.Checkout) error {
	path := filepath.Join(co.Root, "supabase", ".gitignore")
	work, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			work = nil
		} else {
			return err
		}
	}
	if index, err := exec.Command("git", "-C", co.Root, "show", ":supabase/.gitignore").Output(); err == nil && work != nil {
		if rest, ok := bytes.CutPrefix(work, index); ok && onlyCLILines(rest) && len(rest) > 0 {
			if err := os.WriteFile(path, index, 0o644); err != nil {
				return err
			}
		}
	}
	return ensureExclude(co.CommonDir)
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
