package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"
)

// throttleCmd is `hydra throttle NAME N -- COMMAND...`: it runs COMMAND once
// fewer than N commands of the same NAME run on this machine, and waits its
// turn otherwise.
//
// It is for the heavy step of a service's command, a compile above all:
// twenty worktrees started at once would otherwise each build at the same
// moment, every build using every core, and the machine stops answering.
// The turns are flocks on files in $TMPDIR, so a command that dies, however
// it dies, gives its turn back.
func throttleCmd(args []string) error {
	if len(args) >= 1 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) < 3 {
		return fmt.Errorf("usage: hydra throttle NAME N -- COMMAND [ARGS...]")
	}
	name, n := args[0], 0
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(name) {
		return fmt.Errorf("hydra throttle: NAME may use letters, digits, '.', '_' and '-' only: %q", name)
	}
	n, err := strconv.Atoi(args[1])
	if err != nil || n < 1 {
		return fmt.Errorf("hydra throttle: N must be a whole number of 1 or more: %q", args[1])
	}
	cmdArgs := args[2:]
	if cmdArgs[0] == "--" {
		cmdArgs = cmdArgs[1:]
	}
	if len(cmdArgs) == 0 {
		return fmt.Errorf("usage: hydra throttle NAME N -- COMMAND [ARGS...]")
	}

	release, waited := takeTurn(name, n)
	defer release()
	if waited > time.Second {
		fmt.Fprintf(os.Stderr, "hydra: waited %s for a turn to run %s (at most %d at a time on this machine)\n", waited.Round(time.Second), name, n)
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	// Whatever asks this process to stop asks the command too.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		for s := range sigs {
			cmd.Process.Signal(s)
		}
	}()
	err = cmd.Wait()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		release()
		os.Exit(exitCode(exit))
	}
	return err
}

func exitCode(e *exec.ExitError) int {
	if ws, ok := e.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return e.ExitCode()
}

// throttlePoll is how often a command waiting for a turn looks again.
var throttlePoll = 200 * time.Millisecond

// takeTurn holds one of n turns named name until release is called, waiting
// for one to come free. It reports how long it waited.
func takeTurn(name string, n int) (release func(), waited time.Duration) {
	start := time.Now()
	for {
		for i := 0; i < n; i++ {
			path := filepath.Join(os.TempDir(), fmt.Sprintf("hydra-throttle-%s-%d.lock", name, i))
			f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
			if err != nil {
				return func() {}, 0 // no turns to be had: run, as without hydra
			}
			if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
				done := false
				return func() {
					if !done {
						done = true
						syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
						f.Close()
					}
				}, time.Since(start)
			}
			f.Close()
		}
		time.Sleep(throttlePoll)
	}
}
