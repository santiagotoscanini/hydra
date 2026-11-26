package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// A worktree that has fallen far behind main still runs, and `hydra status`
// says it is behind as soon as main moved in a service it takes from main.
// Far behind, the merge that brings it back only gets harder: past
// x-hydra behind_warn commits (100), or as soon as merging main would
// conflict, the warning says so and names the files.

// farBehind is a worktree far behind main.
type farBehind struct {
	Behind    int      // commits main's checkout has that it doesn't
	Limit     int      // behind_warn
	Conflicts []string // files merging main into it conflicts in
}

// farBehindOf says whether the worktree at root, at commit head and behind
// commits behind main's mainHead, is far behind; nil when it isn't, or when
// git can't tell.
func farBehindOf(root, head, mainHead string, behind, limit int) *farBehind {
	if behind == 0 || head == "" || mainHead == "" {
		return nil
	}
	conflicts, err := mainConflicts(root, head, mainHead)
	if err != nil {
		return nil // older git, or a commit it can't read: the plain "behind" still shows
	}
	if behind <= limit && len(conflicts) == 0 {
		return nil
	}
	return &farBehind{Behind: behind, Limit: limit, Conflicts: conflicts}
}

// problem is how `hydra status` says it, a warning: the stack still runs.
func (f *farBehind) problem(base string) problem {
	text := fmt.Sprintf("%d commit%s behind %s", f.Behind, plural(f.Behind), base)
	if len(f.Conflicts) > 0 {
		return problem{
			Text: fmt.Sprintf("%s; merging it conflicts in %d file%s: %s", text, len(f.Conflicts), plural(len(f.Conflicts)), someFiles(f.Conflicts)),
			Fix:  "merge " + base + " here and resolve them: git merge " + base + "   (hydra update leaves a conflict alone)",
		}
	}
	return problem{
		Text: fmt.Sprintf("%s (more than behind_warn, %d): the longer it waits, the harder the merge.", text, f.Limit),
		Fix:  "hydra update --here",
	}
}

// mergeTrees caches mainConflicts by the pair of commits: git merge-tree
// reads both trees, which takes seconds on a large repository, and its
// answer for two commits never changes.
var mergeTrees = struct {
	sync.Mutex
	m map[string][]string
}{m: map[string][]string{}}

// mainConflicts lists the files that merging mainHead into head would
// conflict in, with git merge-tree (git 2.38 or later), which merges in
// memory and leaves the worktree alone.
func mainConflicts(root, head, mainHead string) ([]string, error) {
	key := head + " " + mainHead
	mergeTrees.Lock()
	files, ok := mergeTrees.m[key]
	mergeTrees.Unlock()
	if ok {
		return files, nil
	}
	_, files, err := mergeTree(root, head, mainHead)
	if err != nil {
		return nil, err
	}
	mergeTrees.Lock()
	if len(mergeTrees.m) > 1000 { // twenty worktrees, each at many commits over a long watch
		mergeTrees.m = map[string][]string{}
	}
	mergeTrees.m[key] = files
	mergeTrees.Unlock()
	return files, nil
}

// mergeTree merges theirs into ours in memory, with git merge-tree (git
// 2.38 or later), and returns the merged tree and the files that conflict
// (empty, not nil, when none do). The first line of its output is the tree,
// then the conflicting files. Exit 1 is a conflict only with a tree: git
// also exits 1 for a commit it can't read, which is an error here, never a
// conflict in no files.
func mergeTree(root, ours, theirs string) (tree string, conflicts []string, err error) {
	out, err := exec.Command("git", "-C", root, "merge-tree", "--write-tree", "--name-only", "--no-messages", ours, theirs).Output()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var exit *exec.ExitError
	switch {
	case !isObjectID(lines[0]) && err == nil:
		return "", nil, fmt.Errorf("git merge-tree: no tree in what it printed: %q", lastLine(out))
	case !isObjectID(lines[0]):
		if msg := lastLine(stderrOf(err)); msg != "" {
			return "", nil, fmt.Errorf("git merge-tree: %v: %s", err, msg)
		}
		return "", nil, fmt.Errorf("git merge-tree: %v", err)
	case err == nil:
		return lines[0], []string{}, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return lines[0], append([]string{}, lines[1:]...), nil
	default:
		return "", nil, fmt.Errorf("git merge-tree: %w", err)
	}
}

func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func stderrOf(err error) []byte {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.Stderr
	}
	return nil
}
