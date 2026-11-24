package main

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/santiagotoscanini/hydra/internal/config"
)

// holder is the program listening on a port.
type holder struct {
	Port int
	Pid  int
	Cmd  string
	Dir  string // its working directory, "" when unknown
}

// portInUse matches what servers print when their port is taken: Go's
// "bind: address already in use", Node's EADDRINUSE, and plain sentences.
var portInUse = regexp.MustCompile(`(?i)address already in use|EADDRINUSE|in use by another process|port \d+ is (already )?in use`)

// portTaken explains a service that failed because its port was taken: it
// names the program that holds one of the service's ports in this slot. A
// service that died for another reason gets nothing, and so does one whose
// ports are free. Asking costs an lsof per port, so it is only asked once
// the service's own output says a port was in use.
func portTaken(st *config.Stack, source string, slot int, output []string) *holder {
	s := st.Services[source]
	if s == nil || !portInUse.MatchString(strings.Join(output, "\n")) {
		return nil
	}
	var ports []int
	for _, p := range st.SlotPorts(s, slot) {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	for _, p := range ports {
		if h := listening(p); h != nil {
			return h
		}
	}
	return nil
}

// listening finds who listens on a TCP port of this machine. nil when
// nobody does, or when lsof isn't there to ask.
func listening(port int) *holder {
	out := lsof("-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fpc")
	h := &holder{Port: port}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "p") && h.Pid == 0:
			h.Pid, _ = strconv.Atoi(line[1:])
		case strings.HasPrefix(line, "c") && h.Cmd == "":
			h.Cmd = line[1:]
		}
	}
	if h.Pid == 0 {
		return nil
	}
	for _, line := range strings.Split(lsof("-a", "-p", strconv.Itoa(h.Pid), "-d", "cwd", "-Fn"), "\n") {
		if strings.HasPrefix(line, "n") {
			h.Dir = line[1:]
		}
	}
	return h
}

func lsof(args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "lsof", args...).Output() // exits 1 when nothing matches
	return string(out)
}

// takenBy words the holder of a service's port for a problem, and what to do
// about it. root is the checkout, restart the command that starts the
// service again.
//
//   - The service's own earlier copy (see leftovers): the restart stops it
//     first, so the restart is the whole fix.
//   - Something else that runs in this checkout, a dev server started by
//     hand, say: named, with the command that ends it.
//   - Anything else is someone else's program, and is only named.
func takenBy(h *holder, st *config.Stack, slot int, name, root, restart string) (text, fix string) {
	who := fmt.Sprintf("%s (pid %d)", h.Cmd, h.Pid)
	for _, l := range leftovers(st, slot, []string{name}, []string{root}) {
		if containsInt(l.Tree, h.Pid) {
			return fmt.Sprintf("\n  Port %d is held by %s, left over from an earlier run of this stack.", h.Port, who),
				restart + "   (stops the leftover first)"
		}
	}
	if h.Dir != "" && inside(h.Dir, root) {
		return fmt.Sprintf("\n  Port %d is held by %s, which runs in this checkout but not as part of its stack.", h.Port, who),
			fmt.Sprintf("kill %d && %s", h.Pid, restart)
	}
	return fmt.Sprintf("\n  Port %d is held by another program: %s.", h.Port, who),
		fmt.Sprintf("stop %s, then: %s", h.Cmd, restart)
}
