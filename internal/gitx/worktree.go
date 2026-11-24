package gitx

import (
	"os/exec"
	"runtime"
)

// WorkTree returns a git command for dir that reads its working tree
// (status, diff against the files, untracked files), with git's own
// file-system monitor answering which files changed where git has one
// (macOS and Windows). Without it git reads every tracked file's metadata
// and every directory on each call: in a 23,000-file repository that is
// about 0.3 s of kernel time, and with a watcher per worktree asking every
// two seconds, twenty worktrees kept three cores busy. With it, 0.01 s.
//
// The monitor is a daemon per working tree (about 6 MB) that git starts on
// the first such call and that stops by itself when the worktree is
// removed. The options apply to hydra's own calls only: the repository's
// configuration is left as it is.
func WorkTree(dir string, args ...string) *exec.Cmd {
	var pre []string
	if fsMonitor {
		pre = []string{"-c", "core.fsmonitor=true", "-c", "core.untrackedCache=true"}
	}
	return exec.Command("git", append(append(pre, "-C", dir), args...)...)
}

var fsMonitor = runtime.GOOS == "darwin" || runtime.GOOS == "windows"
