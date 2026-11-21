// Package slots hands out worktree slots and remembers them, so a worktree
// keeps its ports across runs and two worktrees never share a slot.
//
// The registry is one JSON file in the repository's shared .git directory,
// guarded by an flock, so every worktree of a repo sees the same one.
package slots

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
// 1..max if root has none. Entries whose directory is gone are dropped.
func Acquire(commonDir, root string, max int) (int, error) {
	var slot int
	err := withRegistry(commonDir, func(r *registry) (bool, error) {
		dirty := prune(r)
		if s, ok := r.Slots[root]; ok && s <= max {
			slot = s
			return dirty, nil
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
		return dirty, fmt.Errorf("all %d slots are taken; remove a worktree or raise x-hydra.max_slots", max)
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

// List returns the live entries, sorted by slot.
func List(commonDir string) ([]Entry, error) {
	var out []Entry
	err := withRegistry(commonDir, func(r *registry) (bool, error) {
		dirty := prune(r)
		for root, s := range r.Slots {
			out = append(out, Entry{root, s})
		}
		return dirty, nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out, err
}

func prune(r *registry) bool {
	dirty := false
	for root := range r.Slots {
		if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
			delete(r.Slots, root)
			dirty = true
		}
	}
	return dirty
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
