package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/santiagotoscanini/hydra/internal/journal"
	"github.com/santiagotoscanini/hydra/internal/pc"
)

// process-compose's `down` signals each process and waits for it to exit,
// for ever unless the process has a shutdown.timeout_seconds of its own. A
// process that takes the signal and never exits (a Redis that "schedules"
// its shutdown and then hangs, say) holds the whole stop, with nothing on
// the screen. So hydra runs `down` and watches the stack while it stops: it
// says what it is still waiting for, and kills what is still stopping after
// a while.

// stopGrace is how long a stopping process gets before hydra kills it,
// unless its own shutdown.timeout_seconds is longer.
var stopGrace = 60 * time.Second

// stopTell is how often hydra says what it is waiting for.
var stopTell = 10 * time.Second

// stopPoll is how often hydra asks process-compose how the stop goes.
var stopPoll = time.Second

// downWatched runs `process-compose down` for the stack c and waits for it,
// saying on out what the stop waits for, and killing what takes too long.
func downWatched(pcBin string, args []string, c *pc.Client, out io.Writer) error {
	cmd := exec.Command(pcBin, args...)
	cmd.Args[0] = "process-compose"
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = withoutAgentMarks(os.Environ())
	_, err := watchStop(cmd, c, out)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return errExit(exitCode(exit)) // process-compose said what went wrong
	}
	return err
}

// killed is a process hydra had to kill to end a stop.
type killed struct {
	name   string
	pid    int
	waited time.Duration
}

// watchStop runs cmd, a `process-compose down`, and watches the stack c
// until it ends. It returns the processes it killed.
func watchStop(cmd *exec.Cmd, c *pc.Client, out io.Writer) ([]killed, error) {
	grace := graceFor(c)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	began := time.Now()
	stopping := map[string]time.Time{} // since when each process is stopping
	var kills []killed
	tick := time.NewTicker(stopPoll)
	defer tick.Stop()
	told := began
	for {
		select {
		case err := <-done:
			return kills, err
		case now := <-tick.C:
			procs, err := c.Processes()
			if err != nil {
				continue // the server is on its way out
			}
			var waiting, next []string
			for _, p := range procs {
				if p.Status != "Terminating" {
					delete(stopping, p.Name)
					if p.IsRunning {
						next = append(next, p.Name)
					}
					continue
				}
				since, ok := stopping[p.Name]
				if !ok {
					since, stopping[p.Name] = now, now
				}
				waited := now.Sub(since)
				if waited >= grace(p.Name) && p.Pid > 0 {
					if k, err := killStopping(p, waited); err != nil {
						fmt.Fprintf(out, "hydra: %s (pid %d) is still stopping after %s, and killing it failed: %v\n", p.Name, p.Pid, secs(waited), err)
					} else if k {
						fmt.Fprintf(out, "hydra: %s didn't stop within %s of being asked to; killed it (pid %d and its process group)\n", p.Name, secs(waited), p.Pid)
						kills = append(kills, killed{p.Name, p.Pid, waited})
					}
					continue
				}
				waiting = append(waiting, fmt.Sprintf("%s (%s)", p.Name, secs(waited)))
			}
			if now.Sub(told) >= stopTell && (len(waiting) > 0 || len(next) > 0) {
				told = now
				fmt.Fprintln(out, waitingLine(waiting, next))
			}
		}
	}
}

// waitingLine says what a stop waits for: the processes that were asked to
// stop, and those whose turn hasn't come.
func waitingLine(waiting, next []string) string {
	sort.Strings(waiting)
	sort.Strings(next)
	switch {
	case len(waiting) == 0:
		return "hydra: stopping; not asked to stop yet: " + strings.Join(next, ", ")
	case len(next) == 0:
		return "hydra: waiting for " + strings.Join(waiting, ", ")
	}
	return "hydra: waiting for " + strings.Join(waiting, ", ") + "; then " + strings.Join(next, ", ")
}

// secs is a duration as a person reads it in this message: "40s".
func secs(d time.Duration) string { return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds())) }

// killStopping sends SIGKILL to a process that won't stop, and to its
// process group: process-compose starts each process in a group of its own
// (pgid = pid), and what the shell started is in it too. It reports false
// when the process was already gone.
func killStopping(p pc.Process, waited time.Duration) (bool, error) {
	target := p.Pid
	pgid, err := syscall.Getpgid(p.Pid)
	switch {
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	case err != nil:
		return false, err
	case pgid == p.Pid:
		target = -p.Pid
	}
	if err := syscall.Kill(target, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return false, err
	}
	journal.Log("killed", journal.Fields{"name": p.Name, "pid": p.Pid, "waited_s": int(waited.Seconds()), "why": "didn't stop on hydra down"})
	return true, nil
}

// graceFor is how long each process of the stack c gets to stop: stopGrace,
// or longer if its shutdown.timeout_seconds says so (process-compose kills
// it itself then, and hydra comes in only if that didn't work).
func graceFor(c *pc.Client) func(name string) time.Duration {
	own := map[string]time.Duration{}
	if files, err := c.Files(); err == nil {
		for _, f := range files {
			for name, d := range shutdownTimeouts(f) {
				own[name] = d
			}
		}
	}
	return func(name string) time.Duration {
		if d := own[name] + 10*time.Second; own[name] > 0 && d > stopGrace {
			return d
		}
		return stopGrace
	}
}

// shutdownTimeouts reads each process's shutdown.timeout_seconds from a
// process-compose file. A file that can't be read gives none.
func shutdownTimeouts(file string) map[string]time.Duration {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var doc struct {
		Processes map[string]struct {
			Shutdown struct {
				Timeout int `yaml:"timeout_seconds"`
			} `yaml:"shutdown"`
		} `yaml:"processes"`
	}
	if yaml.Unmarshal(data, &doc) != nil {
		return nil
	}
	out := map[string]time.Duration{}
	for name, p := range doc.Processes {
		if p.Shutdown.Timeout > 0 {
			out[name] = time.Duration(p.Shutdown.Timeout) * time.Second
		}
	}
	return out
}

// killedNote names, after "stopped", what had to be killed.
func killedNote(kills []killed) string {
	if len(kills) == 0 {
		return ""
	}
	var names []string
	for _, k := range kills {
		names = append(names, k.name)
	}
	return fmt.Sprintf(" (killed %s, which didn't stop within %s)", strings.Join(names, ", "), secs(kills[0].waited))
}
