// Package routes is how a worktree tells the baseline's router what it runs.
//
// Each worktree that runs hydra writes one small file into the repository's
// shared .git directory. The router reads them all: which ports to open for
// the worktree, and which of its own services calls for that worktree go to.
package routes

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

const dirName = "hydra-routes"

// Entry is a routed service a worktree reaches through its own port.
type Entry struct {
	Port int `json:"port"`
	// Upstreams are the values the service must pick per request itself,
	// by env var name.
	Upstreams map[string]string `json:"upstreams,omitempty"`
}

// File is one worktree's routes.
type File struct {
	Slot   int    `json:"slot"`
	Name   string `json:"name"`           // branch, for messages
	Host   string `json:"host,omitempty"` // its name under .localhost (x-hydra hosts)
	Root   string `json:"root"`           // worktree root
	Socket string `json:"socket"`         // its process-compose server; routes hold while it answers
	// Local maps each service this worktree runs to its port.
	Local map[string]int `json:"local"`
	// Entries maps each routed service the worktree didn't change to the
	// port it reaches the baseline's copy through.
	Entries map[string]Entry `json:"entries"`
}

// Tag is the value of the X-Hydra-Worktree header for this worktree.
func (f File) Tag() string { return strconv.Itoa(f.Slot) }

// Live reports whether the worktree's process-compose server answers.
func (f File) Live() bool {
	c, err := net.DialTimeout("unix", f.Socket, 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func path(commonDir string, slot int) string {
	return filepath.Join(commonDir, dirName, "slot-"+strconv.Itoa(slot)+".json")
}

// Write stores f, and reports whether it differs from what was there.
func Write(commonDir string, f File) (bool, error) {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return false, err
	}
	data = append(data, '\n')
	p := path(commonDir, f.Slot)
	if current, err := os.ReadFile(p); err == nil && bytes.Equal(current, data) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, p) // readers never see half a file
}

// Remove drops a slot's routes.
func Remove(commonDir string, slot int) error {
	err := os.Remove(path(commonDir, slot))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Read returns every worktree's routes, sorted by slot. Files of worktrees
// that no longer exist are deleted.
func Read(commonDir string) ([]File, error) {
	names, err := filepath.Glob(filepath.Join(commonDir, dirName, "slot-*.json"))
	if err != nil {
		return nil, err
	}
	var out []File
	for _, name := range names {
		data, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		var f File
		if json.Unmarshal(data, &f) != nil || f.Slot == 0 {
			continue
		}
		if _, err := os.Stat(f.Root); errors.Is(err, os.ErrNotExist) {
			os.Remove(name)
			continue
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out, nil
}
