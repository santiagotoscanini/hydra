// Package tunnels remembers which checkout holds each tunnel (x-hydra
// tunnel): one JSON file in the repository's shared .git directory, guarded
// by an flock like the slot registry, so every worktree sees the same one.
// A tunnel with no entry is main's.
package tunnels

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

const fileName = "hydra-tunnels.json"

// Holder is the worktree a tunnel's requests go to.
type Holder struct {
	Root   string    `json:"root"`
	Branch string    `json:"branch,omitempty"`
	Since  time.Time `json:"since"`
}

type registry struct {
	Tunnels map[string]Holder `json:"tunnels"` // tunnel name -> holder
}

// Read returns every tunnel a worktree holds. It only reads: a repository
// where no tunnel was ever taken has no file, and gets none from a look.
func Read(commonDir string) (map[string]Holder, error) {
	out := map[string]Holder{}
	if _, err := os.Stat(filepath.Join(commonDir, fileName)); commonDir == "" || errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	err := withRegistry(commonDir, func(r *registry) (bool, error) {
		for name, h := range r.Tunnels {
			out[name] = h
		}
		return false, nil
	})
	return out, err
}

// Take gives tunnel name to h. prev is who had it, the zero Holder for main.
func Take(commonDir, name string, h Holder) (prev Holder, err error) {
	err = withRegistry(commonDir, func(r *registry) (bool, error) {
		prev = r.Tunnels[name]
		if prev.Root == h.Root {
			return false, nil // it has it already, since then
		}
		r.Tunnels[name] = h
		return true, nil
	})
	return prev, err
}

// Release gives tunnel name back to main. prev is who had it, the zero
// Holder when main did.
func Release(commonDir, name string) (prev Holder, err error) {
	err = withRegistry(commonDir, func(r *registry) (bool, error) {
		prev = r.Tunnels[name]
		_, had := r.Tunnels[name]
		delete(r.Tunnels, name)
		return had, nil
	})
	return prev, err
}

// ReleaseRoot gives every tunnel the worktree at root holds back to main,
// and returns their names, sorted.
func ReleaseRoot(commonDir, root string) ([]string, error) {
	held, err := Read(commonDir)
	if err != nil || len(held) == 0 {
		return nil, err
	}
	var names []string
	err = withRegistry(commonDir, func(r *registry) (bool, error) {
		for name, h := range r.Tunnels {
			if h.Root == root {
				names = append(names, name)
				delete(r.Tunnels, name)
			}
		}
		return len(names) > 0, nil
	})
	sort.Strings(names)
	return names, err
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

	r := &registry{Tunnels: map[string]Holder{}}
	if info, _ := f.Stat(); info != nil && info.Size() > 0 {
		if err := json.NewDecoder(f).Decode(r); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if r.Tunnels == nil {
			r.Tunnels = map[string]Holder{}
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
