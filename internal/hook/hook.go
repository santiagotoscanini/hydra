// Package hook carries the Node preload that lets one copy of a service
// serve several worktrees (see hook.cjs).
package hook

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed hook.cjs
var source []byte

// Install writes the hook into dir and returns its path. The file is only
// rewritten when its content differs, so a running service keeps a stable
// path across hydra upgrades.
func Install(dir string) (string, error) {
	path := filepath.Join(dir, "hydra-hook.cjs")
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, source) {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, source, 0o644)
}
