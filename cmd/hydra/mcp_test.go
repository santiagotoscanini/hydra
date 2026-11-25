package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/santiagotoscanini/hydra/internal/gitx"
)

const mcpStack = `version: "0.5"
x-hydra:
  slot_stride: 100
  max_slots: 4
processes:
  db:
    command: sleep 1000
    x-hydra: { shared: true, port: 25030 }
  api:
    command: sleep 1000
    depends_on:
      db: { condition: process_started }
    x-hydra: { port: 25010, paths: ["apps/api/**"], upstreams: { DATABASE_URL: db } }
`

// mcpRepo makes a main checkout with a stack file and a worktree that
// changed the api. No stack runs: main's process-compose would be on a port
// nothing listens on.
func mcpRepo(t *testing.T) (main, wt string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PC_PORT_NUM", strconv.Itoa(l.Addr().(*net.TCPAddr).Port))
	t.Setenv("PC_ADDRESS", "127.0.0.1")
	l.Close()
	dir := t.TempDir()
	main, wt = filepath.Join(dir, "main"), filepath.Join(dir, "wt")
	git := func(in string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", in}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	os.MkdirAll(filepath.Join(main, "apps", "api"), 0o755)
	git(main, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(main, "process-compose.yaml"), []byte(mcpStack), 0o644)
	os.WriteFile(filepath.Join(main, ".gitignore"), []byte(".process-compose/\n"), 0o644)
	os.WriteFile(filepath.Join(main, "apps", "api", "a.go"), []byte("a"), 0o644)
	git(main, "add", ".")
	git(main, "commit", "-q", "-m", "init")
	git(main, "worktree", "add", "-q", "-b", "feature", wt)
	os.WriteFile(filepath.Join(wt, "apps", "api", "b.go"), []byte("b"), 0o644)
	git(wt, "add", ".")
	git(wt, "commit", "-q", "-m", "api")
	return main, wt
}

// session sends the lines to a server in dir and returns its replies by id,
// and everything it wrote.
func session(t *testing.T, dir string, lines ...string) (map[string]map[string]any, string) {
	t.Helper()
	var out bytes.Buffer
	s := newMCPServer(dir, &out)
	if err := s.serve(strings.NewReader(strings.Join(lines, "\n") + "\n")); err != nil {
		t.Fatal(err)
	}
	replies := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON on stdout: %q", line)
		}
		replies[fmt.Sprint(m["id"])] = m
	}
	return replies, out.String()
}

func request(id int, method, params string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, id, method, params)
}

func toolCall(id int, name, args string) string {
	return request(id, "tools/call", fmt.Sprintf(`{"name":%q,"arguments":%s}`, name, args))
}

// toolText is a tools/call result's first text, and whether it is an error.
func toolText(t *testing.T, reply map[string]any) (string, bool) {
	t.Helper()
	r, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", reply)
	}
	content := r["content"].([]any)
	isErr, _ := r["isError"].(bool)
	return content[0].(map[string]any)["text"].(string), isErr
}

func TestMCPProtocol(t *testing.T) {
	r, raw := session(t, t.TempDir(),
		request(1, "initialize", `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}`),
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		request(2, "initialize", `{"protocolVersion":"2024-11-05"}`),
		request(3, "initialize", `{"protocolVersion":"1999-01-01"}`),
		request(4, "ping", `{}`),
		request(5, "tools/list", `{}`),
		request(6, "resources/list", `{}`),
		request(7, "tools/call", `{"name":"nope","arguments":{}}`),
		`{not json`,
	)
	if n := strings.Count(raw, "\n"); n != 8 { // the notification gets no answer
		t.Errorf("%d replies, want 8:\n%s", n, raw)
	}
	version := func(id string) any { return r[id]["result"].(map[string]any)["protocolVersion"] }
	if v := version("1"); v != "2025-06-18" {
		t.Errorf("protocolVersion %v", v)
	}
	if v := version("2"); v != "2024-11-05" {
		t.Errorf("a known older version is kept: got %v", v)
	}
	if v := version("3"); v != mcpProtocol {
		t.Errorf("an unknown version gets ours: got %v", v)
	}
	if info := r["1"]["result"].(map[string]any)["serverInfo"].(map[string]any); info["name"] != "hydra" {
		t.Errorf("serverInfo %v", info)
	}
	if _, ok := r["4"]["result"].(map[string]any); !ok {
		t.Errorf("ping: %v", r["4"])
	}
	var names []string
	for _, tool := range r["5"]["result"].(map[string]any)["tools"].([]any) {
		tl := tool.(map[string]any)
		names = append(names, tl["name"].(string))
		if tl["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("%s: inputSchema %v", tl["name"], tl["inputSchema"])
		}
	}
	if got := strings.Join(names, " "); got != "status up wait down restart logs update_here main_status" {
		t.Errorf("tools: %s", got)
	}
	code := func(id string) any { return r[id]["error"].(map[string]any)["code"] }
	if c := code("6"); c != float64(rpcNoMethod) {
		t.Errorf("unknown method: %v", r["6"])
	}
	if c := code("7"); c != float64(rpcInvalidParams) {
		t.Errorf("unknown tool: %v", r["7"])
	}
	if c := code("<nil>"); c != float64(rpcParseError) {
		t.Errorf("bad JSON: %v", r["<nil>"])
	}
}

func TestMCPToolsInAWorktree(t *testing.T) {
	_, wt := mcpRepo(t)
	r, _ := session(t, wt,
		toolCall(1, "status", `{}`),
		toolCall(2, "logs", `{"process":"api"}`),
		toolCall(3, "status", `{"verbose":true}`),
		toolCall(4, "wait", `{}`),
		toolCall(5, "main_status", `{}`),
	)
	text, isErr := toolText(t, r["1"])
	if isErr || !strings.Contains(text, "stack not started") || !strings.Contains(text, "api  here") {
		t.Errorf("status:\n%s", text)
	}
	// main's stack is down: that is the problem status names, with its fix.
	if !strings.Contains(text, "✗ ") {
		t.Errorf("status names no problem with main down:\n%s", text)
	}
	data := r["1"]["result"].(map[string]any)["structuredContent"].(map[string]any)
	w := data["worktree"].(map[string]any)
	if w["up"] != false || w["branch"] != "feature" || len(w["problems"].([]any)) == 0 {
		t.Errorf("structured status: %v", w)
	}
	if text, isErr := toolText(t, r["2"]); !isErr || !strings.Contains(text, "this worktree's stack isn't running") {
		t.Errorf("logs of a stack that isn't up: %v %s", isErr, text)
	}
	if text, isErr := toolText(t, r["3"]); !isErr || !strings.Contains(text, `unknown field "verbose"`) {
		t.Errorf("an argument the tool doesn't take: %v %s", isErr, text)
	}
	if text, isErr := toolText(t, r["4"]); !isErr || !strings.Contains(text, "start it with the up tool") {
		t.Errorf("wait on a stack that isn't up: %v %s", isErr, text)
	}
	text, isErr = toolText(t, r["5"])
	if isErr || !strings.Contains(text, "stack DOWN") || !strings.Contains(text, "db  main is down  (api here calls it)") {
		t.Errorf("main_status:\n%s", text)
	}
}

func TestMCPRefusesToChangeMainsStack(t *testing.T) {
	main, _ := mcpRepo(t)
	r, _ := session(t, main,
		toolCall(1, "up", `{}`),
		toolCall(2, "down", `{}`),
		toolCall(3, "restart", `{"process":"api"}`),
		toolCall(4, "update_here", `{}`),
		toolCall(5, "status", `{}`),
	)
	for _, id := range []string{"1", "2", "3", "4"} {
		text, isErr := toolText(t, r[id])
		if !isErr || !strings.Contains(text, "Ask the developer: main's stack is theirs") {
			t.Errorf("call %s in main: %v %s", id, isErr, text)
		}
		if refused := r[id]["result"].(map[string]any)["structuredContent"].(map[string]any)["refused"]; refused != true {
			t.Errorf("call %s: refused is %v", id, refused)
		}
	}
	if text, isErr := toolText(t, r["5"]); isErr || !strings.Contains(text, "stack DOWN") {
		t.Errorf("status in main reads it: %v %s", isErr, text)
	}
	// And none of it started anything.
	co, err := gitx.Find(main)
	if err != nil {
		t.Fatal(err)
	}
	if ownServer(co, nil).Alive() {
		t.Error("something answers where main's stack would")
	}
}

// Whatever hydra's code prints while a tool runs goes to stderr: stdout
// carries protocol messages only.
func TestMCPStdoutCarriesOnlyMessages(t *testing.T) {
	mcpTools = append(mcpTools, mcpTool{name: "noisy", schema: noArgs, run: func(context.Context, *mcpCall) (toolResult, error) {
		fmt.Println("a stray line")
		printProblems(os.Stdout, []problem{{Text: "printed by a check"}})
		return toolResult{summary: "done", data: map[string]any{}}, nil
	}})
	defer func() { mcpTools = mcpTools[:len(mcpTools)-1] }()

	_, wt := mcpRepo(t) // a tool runs in a checkout
	stdout, stderr := os.Stdout, os.Stderr
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	os.Stdout, os.Stderr = outW, errW
	err := serveMCP(wt, strings.NewReader(request(1, "tools/list", `{}`)+"\n"+toolCall(2, "noisy", `{}`)+"\n"))
	os.Stdout, os.Stderr = stdout, stderr
	outW.Close()
	errW.Close()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(outR)
	said, _ := io.ReadAll(errR)
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout:\n%s", got)
	}
	for _, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil || m["jsonrpc"] != "2.0" {
			t.Errorf("not a JSON-RPC message on stdout: %q", line)
		}
	}
	if !strings.Contains(string(said), "a stray line") || !strings.Contains(string(said), "printed by a check") {
		t.Errorf("the noise didn't go to stderr: %q", said)
	}
}
