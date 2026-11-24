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

// Acquire returns the slot for root, allocating the lowest free one in
// 1..max if root has none.
func Acquire(commonDir, root string, max int) (int, error) {
	var slot int
	err := withRegistry(commonDir, func(r *registry) (bool, error) {
		if s, ok := r.Slots[root]; ok && s <= max {
			slot = s
			return false, nil
		}
		used := map[int]bool{}
		for _, s := range r.Slots {
			used[s] = true
		}
		for s := 1; s <= max; s++ {
			if !used[s] {
				r.Slots[root] = s
				slot = s
				return true, nil
			}
		}
		return false, fmt.Errorf("all %d slots are taken; free one (`hydra slots` lists them, `hydra slots free PATH` frees one) or raise x-hydra.max_slots", max)
	})
	return slot, err
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
