// Package slots hands out worktree slots and remembers them, so a worktree
// keeps its ports across runs and two worktrees never share a slot.
//
// The registry is one JSON file in the repository's shared .git directory,
// guarded by an flock, so every worktree of a repo sees the same one.
//
// A slot stays taken until it is released. An entry whose folder is no longer
// a worktree (see Removed) may have left a stack running on that slot's
// ports: the caller cleans up after it and then releases the slot (see
// Gone), so the ports are never handed on while something still holds them.
package slots

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const fileName = "hydra-slots.json"

type registry struct {
	Slots map[string]int `json:"slots"` // worktree root -> slot
}

// Entry is one row of List.
type Entry struct {
	Root string
	Slot int
}

// Rules say which slots a worktree may get. Only Max is required.
type Rules struct {
	Max int // slots are 1..Max
	// Skip rules a slot out for good (a port a browser refuses): it is never
	// handed out, and a worktree that holds one gets another.
	Skip func(slot int) bool
	// Busy rules a slot out for a worktree that doesn't hold it yet:
	// something already listens on one of its ports. A worktree that holds
	// it keeps it, since what listens may be its own stack.
	Busy func(slot int) bool
	// Idle says a holder's stack isn't running, so that its slot may be taken
	// when every slot is held; nil: none is.
	Idle func(root string) bool
	// Since orders idle holders: the one idle the longest gives way first.
	Since func(root string) time.Time
}

func (rl Rules) skip(s int) bool { return rl.Skip != nil && rl.Skip(s) }

// open reports whether slot s may go to a worktree that doesn't hold it.
func (rl Rules) open(s int) bool { return !rl.skip(s) && (rl.Busy == nil || !rl.Busy(s)) }

// Acquire returns the slot for root, allocating the lowest open one if root
// has none.
//
// When every slot is taken, it takes one from a worktree whose stack isn't
// running (rules.Idle): the one idle the longest, by rules.Since. That
// worktree gets a slot again, possibly another, the next time it starts. A
// slot then counts stacks that run, not worktrees that once ran. took is the
// root the slot was taken from, "" when none was.
func Acquire(commonDir, root string, rules Rules) (slot int, took string, err error) {
	max := rules.Max
	err = withRegistry(commonDir, func(r *registry) (bool, error) {
		if s, ok := r.Slots[root]; ok && s <= max && !rules.skip(s) {
			slot = s
			return false, nil
		}
		delete(r.Slots, root) // over max, or a slot to skip: it gets another
		if s := lowestFree(r, rules); s > 0 {
			r.Slots[root] = s
			slot = s
			return true, nil
		}
		if rules.Idle != nil {
			var best string
			var bestAt time.Time
			for other, s := range r.Slots {
				if s > max || other == root || !rules.open(s) || !rules.Idle(other) {
					continue
				}
				at := time.Time{}
				if rules.Since != nil {
					at = rules.Since(other)
				}
				if best == "" || at.Before(bestAt) || (at.Equal(bestAt) && other < best) {
					best, bestAt = other, at
				}
			}
			if best != "" {
				slot, took = r.Slots[best], best
				delete(r.Slots, best)
				r.Slots[root] = slot
				return true, nil
			}
		}
		return false, fmt.Errorf("all %d slots are taken by stacks that run; stop one (`hydra down` there, or `hydra down --worktrees`) or raise x-hydra.max_slots", max)
	})
	return slot, took, err
}

// Peek returns the slot root has, or the one Acquire would give it now,
// without taking it: 0 when every slot is taken.
func Peek(commonDir, root string, rules Rules) int {
	all, _ := List(commonDir)
	r := &registry{Slots: map[string]int{}}
	for _, e := range all {
		if e.Root == root && e.Slot <= rules.Max && !rules.skip(e.Slot) {
			return e.Slot
		}
		if e.Root != root {
			r.Slots[e.Root] = e.Slot
		}
	}
	return lowestFree(r, rules)
}

// lowestFree is the lowest open slot that nobody holds, 0 if none.
func lowestFree(r *registry, rules Rules) int {
	used := map[int]bool{}
	for _, s := range r.Slots {
		used[s] = true
	}
	for s := 1; s <= rules.Max; s++ {
		if !used[s] && rules.open(s) {
			return s
		}
	}
	return 0
}

// Release frees root's slot.
func Release(commonDir, root string) error {
	return withRegistry(commonDir, func(r *registry) (bool, error) {
		_, ok := r.Slots[root]
		delete(r.Slots, root)
		return ok, nil
	})
}

// List returns every entry, sorted by slot. It only reads: a repository
// that never handed out a slot has no registry, and gets none from a look.
func List(commonDir string) ([]Entry, error) {
	var out []Entry
	if _, err := os.Stat(filepath.Join(commonDir, fileName)); commonDir == "" || errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	err := withRegistry(commonDir, func(r *registry) (bool, error) {
		for root, s := range r.Slots {
			out = append(out, Entry{root, s})
		}
		return false, nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out, err
}

// Gone returns the entries whose worktree was removed and that still hold
// their slot.
func Gone(commonDir string) ([]Entry, error) {
	all, err := List(commonDir)
	var out []Entry
	for _, e := range all {
		if e.Missing() {
			out = append(out, e)
		}
	}
	return out, err
}

// Missing reports whether the entry's worktree was removed.
func (e Entry) Missing() bool { return Removed(e.Root) }

// Removed reports whether the folder at root has stopped being a git
// worktree: its `.git` is gone, or names a git directory that is gone.
//
// Whether the folder exists doesn't say. git forgets a worktree even when
// it can't delete all of it, and one removed while its stack runs often
// can't be deleted: what is left is a folder with a few new files in it, or
// most of the checkout with a `.git` that leads nowhere.
func Removed(root string) bool {
	dotGit := filepath.Join(root, ".git")
	info, err := os.Stat(dotGit)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil || info.IsDir() {
		return false // can't tell, or a checkout of its own
	}
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return false
	}
	dir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return false
	}
	if dir = strings.TrimSpace(dir); !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	_, err = os.Stat(dir)
	return errors.Is(err, os.ErrNotExist)
}

func withRegistry(commonDir string, fn func(*registry) (bool, error)) error {
	path := filepath.Join(commonDir, fileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	r := &registry{Slots: map[string]int{}}
	if info, _ := f.Stat(); info != nil && info.Size() > 0 {
		if err := json.NewDecoder(f).Decode(r); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if r.Slots == nil {
			r.Slots = map[string]int{}
		}
	}
	dirty, fnErr := fn(r)
	if dirty {
		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		if err := f.Truncate(0); err != nil {
			return err
		}
		if _, err := f.WriteAt(append(data, '\n'), 0); err != nil {
			return err
		}
	}
	return fnErr
}
