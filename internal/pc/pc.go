// Package pc talks to a running process-compose server over its HTTP API:
// the main checkout's on a TCP port, a worktree's on a unix socket.
package pc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client reaches one process-compose server.
type Client struct {
	base string
	http *http.Client
	// Where is how to describe the server in a message: "127.0.0.1:8099",
	// or the socket's path.
	Where string
}

// TCP returns a client for a server on host:port.
func TCP(host string, port int) *Client {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	return &Client{base: "http://" + addr, Where: addr, http: &http.Client{Timeout: 10 * time.Second}}
}

// Unix returns a client for a server on a unix socket.
func Unix(socket string) *Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &Client{base: "http://unix", Where: socket, http: &http.Client{Timeout: 10 * time.Second, Transport: tr}}
}

// Process is one process's state, as process-compose reports it.
type Process struct {
	Name          string `json:"name"`
	Status        string `json:"status"`   // Running, Completed, Disabled, Pending, Launched, Restarting, ...
	IsReady       string `json:"is_ready"` // "Ready", "Not Ready" or "-"
	HasReadyProbe bool   `json:"has_ready_probe"`
	Restarts      int    `json:"restarts"`
	ExitCode      int    `json:"exit_code"`
	Pid           int    `json:"pid"`
	IsRunning     bool   `json:"is_running"`
	StartTime     string `json:"process_start_time"`
}

// Health sums a process's state up for a person.
type Health int

const (
	Up       Health = iota // running, and ready if it says when it is
	Done                   // ran and exited 0
	Starting               // running, not ready yet
	Off                    // disabled, or never started
	Failed                 // exited non-zero, or gave up restarting
)

// Health classifies the state.
func (p Process) Health() Health {
	switch p.Status {
	case "Disabled", "Skipped", "Foreground":
		return Off
	case "Completed":
		if p.ExitCode == 0 {
			return Done
		}
		return Failed
	case "Error":
		return Failed
	case "Running", "Launched":
		if p.IsReady == "Not Ready" {
			return Starting
		}
		return Up
	case "Terminating":
		return Off
	}
	return Starting // Pending, Launching, Restarting
}

// Describe is the state in a few words: "ready", "exited 1 (3 restarts)".
func (p Process) Describe() string {
	restarts := ""
	if p.Restarts > 0 {
		restarts = fmt.Sprintf(", restarted %d×", p.Restarts)
	}
	switch p.Health() {
	case Up:
		if p.IsReady == "Ready" {
			return "ready"
		}
		return "running"
	case Done:
		return "done"
	case Off:
		if p.Status == "Disabled" {
			return "off"
		}
		return strings.ToLower(p.Status)
	case Failed:
		return fmt.Sprintf("exited with code %d%s", p.ExitCode, restarts)
	}
	if p.Status == "Running" || p.Status == "Launched" {
		return "starting" + restarts
	}
	return strings.ToLower(p.Status) + restarts
}

// Alive reports whether the server answers.
func (c *Client) Alive() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", c.base+"/live", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Processes lists every process of the project.
func (c *Client) Processes() ([]Process, error) {
	var out struct {
		Data []Process `json:"data"`
	}
	if err := c.do("GET", "/processes", &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// Process returns one process's state.
func (c *Client) Process(name string) (Process, error) {
	var p Process
	err := c.do("GET", "/process/"+url.PathEscape(name), &p)
	return p, err
}

// Restart stops a process and starts it again (or runs it again, if it had
// exited).
func (c *Client) Restart(name string) error {
	return c.do("POST", "/process/restart/"+url.PathEscape(name), nil)
}

// Start starts a process that isn't running.
func (c *Client) Start(name string) error {
	return c.do("POST", "/process/start/"+url.PathEscape(name), nil)
}

// Logs returns the last n lines a process wrote.
func (c *Client) Logs(name string, n int) ([]string, error) {
	var out struct {
		Logs []string `json:"logs"`
	}
	err := c.do("GET", fmt.Sprintf("/process/logs/%s/0/%d", url.PathEscape(name), n), &out)
	return out.Logs, err
}

// Files returns the config files the running project was started from.
func (c *Client) Files() ([]string, error) {
	var out struct {
		FileNames []string `json:"fileNames"`
	}
	err := c.do("GET", "/project/state", &out)
	return out.FileNames, err
}

func (c *Client) do(method, path string, into any) error {
	req, err := http.NewRequest(method, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return fmt.Errorf("process-compose: %s", e.Error)
		}
		return fmt.Errorf("process-compose: %s %s: %s", method, path, resp.Status)
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(body, into)
}
