package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/pc"
	"github.com/santiagotoscanini/hydra/internal/stack"
)

// `hydra mcp` is an MCP server on stdin and stdout, for coding agents. An
// agent harness starts it once per session, in the agent's worktree, and
// the agent drives that worktree's stack through typed tools instead of a
// shell. Agents given a shell get it wrong in ways that cost the developer:
// they start process-compose by hand (with the plain file, which runs
// everything on main's ports), stop main's stack because theirs is
// "broken", or `supabase stop` the shared database. Here, what changes a
// stack only ever changes the worktree's own; in the main checkout it is
// refused, with a message that sends the agent to the developer.
//
// The actions run hydra itself (`hydra up -D`, `hydra down`, ...), so they
// behave exactly as the commands do, and their output is handed back as it
// was printed. The reads call the code behind `hydra status` directly.
//
// The protocol is JSON-RPC 2.0, one message per line. stdout carries
// nothing else: hydra's own code prints to it in places, so for as long as
// the server runs os.Stdout is stderr, and the messages go to the stdout
// the process started with.

// mcpProtocol is the protocol version this server speaks. A client that
// asks for one of mcpProtocols gets that one back.
const mcpProtocol = "2025-06-18"

var mcpProtocols = map[string]bool{"2024-11-05": true, "2025-03-26": true, "2025-06-18": true}

// JSON-RPC error codes.
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcNoMethod       = -32601
	rpcInvalidParams  = -32602
)

const mcpInstructions = `hydra runs this checkout's dev stack: in a git worktree, only the services the branch changed, wired to the main checkout's copies of the rest.

Use these tools, never a shell, to start, stop or restart the stack: never run process-compose yourself, never stop main's stack, never ` + "`supabase stop`" + ` or ` + "`supabase db reset`" + `.
- status first: what runs here, what this worktree takes from main, what is wrong and the fix.
- up, then wait: start this worktree's stack, and wait until it is ready.
- logs and restart for a process that fails; down when you are done.
- When status says this worktree is behind main, update_here merges main in.
Main's stack belongs to the developer: the tools read it (main_status) but refuse to change it. When main's stack is down or broken, tell the developer what main_status says and let them fix it.
Only when the developer asks to update main and every worktree: the update_all prompt is the workflow (worktrees, update_main, update_worktree).`

func mcpCmd(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("hydra mcp takes no arguments; it serves this checkout, the one its working directory is in")
	}
	return serveMCP(".", os.Stdin)
}

// serveMCP answers on what os.Stdout is when it is called, and sends
// everything else hydra prints to stderr until it returns.
func serveMCP(dir string, in io.Reader) error {
	out := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = out }()
	s := newMCPServer(dir, out)
	return s.serve(in)
}

type mcpServer struct {
	dir   string // where the server was started: the checkout it serves
	exe   string // the hydra the actions run
	outMu sync.Mutex
	out   *json.Encoder
	// One action at a time: an up and a down racing each other would leave
	// the stack in whichever state came last.
	act     sync.Mutex
	mu      sync.Mutex
	cancels map[string]context.CancelFunc // by request id: what notifications/cancelled stops
	wg      sync.WaitGroup
}

func newMCPServer(dir string, out io.Writer) *mcpServer {
	exe, _ := os.Executable()
	return &mcpServer{dir: dir, exe: exe, out: json.NewEncoder(out), cancels: map[string]context.CancelFunc{}}
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *mcpServer) send(msg any) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if err := s.out.Encode(msg); err != nil {
		fmt.Fprintln(os.Stderr, "hydra mcp: writing a reply:", err)
	}
}

func (s *mcpServer) reply(id json.RawMessage, result any) {
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *mcpServer) fail(id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": rpcError{Code: code, Message: msg}})
}

// serve reads messages until in closes. Each request is answered on its
// own goroutine, so a long wait doesn't hold up a ping or a status. When
// in closes, what is still waiting is cancelled; an action under way (a
// start, a stop) is left to finish.
func (s *mcpServer) serve(in io.Reader) error {
	r := bufio.NewReader(in)
	defer func() {
		s.mu.Lock()
		for _, cancel := range s.cancels {
			cancel()
		}
		s.mu.Unlock()
		s.wg.Wait()
	}()
	for {
		line, err := r.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			s.handle(line)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *mcpServer) handle(line []byte) {
	var msg rpcMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		if strings.HasPrefix(strings.TrimSpace(string(line)), "[") {
			s.fail(nil, rpcInvalidRequest, "batches are not supported (MCP 2025-06-18 has none)")
			return
		}
		s.fail(nil, rpcParseError, "parse error: "+err.Error())
		return
	}
	notification := len(msg.ID) == 0
	if msg.JSONRPC != "2.0" || msg.Method == "" {
		if !notification {
			s.fail(msg.ID, rpcInvalidRequest, `invalid request: it needs "jsonrpc": "2.0" and a method`)
		}
		return // a response from the client, or a malformed notification: nothing to say
	}
	if notification {
		if msg.Method == "notifications/cancelled" {
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if json.Unmarshal(msg.Params, &p) == nil {
				s.mu.Lock()
				if cancel := s.cancels[string(p.RequestID)]; cancel != nil {
					cancel()
				}
				s.mu.Unlock()
			}
		}
		return // notifications/initialized and the rest need no answer
	}
	switch msg.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(msg.Params, &p)
		version := mcpProtocol
		if mcpProtocols[p.ProtocolVersion] {
			version = p.ProtocolVersion
		}
		s.reply(msg.ID, map[string]any{
			"protocolVersion": version,
			"capabilities": map[string]any{
				"tools":   map[string]any{"listChanged": false},
				"prompts": map[string]any{"listChanged": false},
			},
			"serverInfo":   map[string]any{"name": "hydra", "version": shortVersion()},
			"instructions": s.instructions(),
		})
	case "ping":
		s.reply(msg.ID, map[string]any{})
	case "tools/list":
		list := make([]map[string]any, 0, len(mcpTools))
		if s.servesHydra() {
			for _, t := range mcpTools {
				list = append(list, t.describe())
			}
		}
		s.reply(msg.ID, map[string]any{"tools": list})
	case "prompts/list":
		list := make([]map[string]any, 0, len(mcpPrompts))
		if s.servesHydra() {
			for _, p := range mcpPrompts {
				list = append(list, p.describe())
			}
		}
		s.reply(msg.ID, map[string]any{"prompts": list})
	case "prompts/get":
		var p struct {
			Name      string            `json:"name"`
			Arguments map[string]string `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			s.fail(msg.ID, rpcInvalidParams, "prompts/get: "+err.Error())
			return
		}
		prompt := findPrompt(p.Name)
		if prompt == nil || !s.servesHydra() {
			s.fail(msg.ID, rpcInvalidParams, fmt.Sprintf("there is no prompt named %q here (prompts/list lists them)", p.Name))
			return
		}
		s.reply(msg.ID, prompt.get(p.Arguments))
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			s.fail(msg.ID, rpcInvalidParams, "tools/call: "+err.Error())
			return
		}
		tool := findTool(p.Name)
		if tool == nil {
			s.fail(msg.ID, rpcInvalidParams, fmt.Sprintf("there is no tool named %q (tools/list lists them)", p.Name))
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		key := string(msg.ID)
		s.mu.Lock()
		s.cancels[key] = cancel
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.cancels, key)
				s.mu.Unlock()
				cancel()
			}()
			s.reply(msg.ID, s.call(ctx, tool, p.Arguments))
		}()
	default:
		s.fail(msg.ID, rpcNoMethod, "method not found: "+msg.Method)
	}
}

// toolResult is what a tool found: a few lines for a person (or an agent)
// to read, and the same as data.
type toolResult struct {
	summary string
	data    any
}

// call runs a tool and turns what it returned into a tools/call result. A
// tool's error is the result's text, as it was, with isError set: the agent
// reads it, which it couldn't if it were a protocol error.
func (s *mcpServer) call(ctx context.Context, tool *mcpTool, raw json.RawMessage) map[string]any {
	res, err := s.run(ctx, tool, raw)
	if err != nil {
		text := err.Error()
		data := map[string]any{"error": text}
		var refused errRefused
		if errors.As(err, &refused) {
			data["refused"] = true
		}
		return map[string]any{"isError": true, "content": []any{textContent(text)}, "structuredContent": data}
	}
	data, merr := json.MarshalIndent(res.data, "", "  ")
	if merr != nil {
		return map[string]any{"isError": true, "content": []any{textContent("encoding the result: " + merr.Error())}}
	}
	return map[string]any{
		"content":           []any{textContent(res.summary), textContent(string(data))},
		"structuredContent": res.data,
	}
}

func textContent(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

func (s *mcpServer) run(ctx context.Context, tool *mcpTool, raw json.RawMessage) (toolResult, error) {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	co, err := gitx.Find(s.dir)
	if err != nil {
		return toolResult{}, fmt.Errorf("hydra mcp serves the checkout its working directory is in, and %s isn't in a git checkout: %w", s.dir, err)
	}
	if tool.changes && !tool.inMain && co.IsMain() {
		return toolResult{}, errRefused(fmt.Sprintf(
			"refused: this is the main checkout (%s). Ask the developer: main's stack is theirs. "+
				"These tools change only a worktree's own stack; main_status, status and logs read main's.", co.Root))
	}
	if tool.changes {
		s.act.Lock()
		defer s.act.Unlock()
	}
	return tool.run(ctx, &mcpCall{server: s, co: co, args: raw})
}

// servesHydra says whether the server's checkout is set up for hydra. A
// server registered for every project (user scope) then offers nothing in
// the others, instead of tools that only fail there.
func (s *mcpServer) servesHydra() bool {
	co, err := gitx.Find(s.dir)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(defaultFile(co))
	return err == nil && config.HasHydra(data)
}

func (s *mcpServer) instructions() string {
	if !s.servesHydra() {
		return "This checkout isn't set up for hydra (no process-compose.yaml with an x-hydra block), so hydra offers no tools here."
	}
	return mcpInstructions
}

// errRefused is an action the server won't take where it was asked.
type errRefused string

func (e errRefused) Error() string { return string(e) }

// mcpCall is one tool call: the checkout and the arguments.
type mcpCall struct {
	server *mcpServer
	co     gitx.Checkout
	args   json.RawMessage
}

// decode reads the arguments into v, refusing ones the tool doesn't take:
// a misspelt argument would otherwise be ignored without a word.
func (c *mcpCall) decode(v any) error {
	d := json.NewDecoder(strings.NewReader(string(c.args)))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("bad arguments: %w", err)
	}
	return nil
}

// hydra runs hydra in the checkout and returns what it printed, stdout and
// stderr as they interleaved. Its output goes to a file, not a pipe: a
// process-compose started with -D keeps what it inherited open, and the
// read of a pipe would only end when the stack stops.
func (c *mcpCall) hydra(args ...string) (string, error) {
	return c.hydraIn(c.co.Root, args...)
}

// hydraIn is hydra run in another checkout of the repository.
func (c *mcpCall) hydraIn(dir string, args ...string) (string, error) {
	f, err := os.CreateTemp("", "hydra-mcp-*.out")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	cmd := exec.Command(c.server.exe, args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = f, f // stdin: /dev/null, so nothing takes a terminal
	runErr := cmd.Run()
	data, err := os.ReadFile(f.Name())
	if err != nil {
		return "", err
	}
	out := strings.TrimRight(string(data), "\n")
	if runErr != nil {
		cmdline := "hydra " + strings.Join(args, " ")
		if out == "" {
			return "", fmt.Errorf("%s failed: %w", cmdline, runErr)
		}
		return out, fmt.Errorf("%s failed (%w):\n%s", cmdline, runErr, out)
	}
	return out, nil
}

// client is the process-compose server of this checkout's stack, or of
// main's when main is true.
func (c *mcpCall) client(main bool) *pc.Client {
	switch {
	case main && !c.co.IsMain():
		return mainClient(c.co.CommonDir)
	case c.co.IsMain():
		return ownServer(c.co, nil)
	}
	return pc.Unix(socketPath(c.co))
}

// notUp is the error for a stack that isn't running, saying who starts it.
func (c *mcpCall) notUp(main bool) error {
	if main || c.co.IsMain() {
		return fmt.Errorf("main's stack isn't running (%s). Ask the developer to start it: it is theirs", c.co.Main)
	}
	return fmt.Errorf("this worktree's stack isn't running (%s); start it with the up tool", c.co.Root)
}

// mcpTool is one tool. changes: it changes a checkout or a stack, so it is
// refused in the main checkout, unless inMain: the update tools, which
// work on other checkouts than the server's and are the same from any.
type mcpTool struct {
	name, title, description     string
	schema                       map[string]any
	changes, inMain, destructive bool
	run                          func(ctx context.Context, c *mcpCall) (toolResult, error)
}

func (t *mcpTool) describe() map[string]any {
	return map[string]any{
		"name": t.name, "title": t.title, "description": t.description, "inputSchema": t.schema,
		"annotations": map[string]any{
			"title":           t.title,
			"readOnlyHint":    !t.changes,
			"destructiveHint": t.destructive,
			"openWorldHint":   false,
		},
	}
}

func findTool(name string) *mcpTool {
	for i := range mcpTools {
		if mcpTools[i].name == name {
			return &mcpTools[i]
		}
	}
	return nil
}

func object(props map[string]any, required ...string) map[string]any {
	o := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

var noArgs = object(map[string]any{})

var mcpTools []mcpTool

func init() {
	mcpTools = []mcpTool{
		{
			name: "status", title: "This checkout's stack",
			description: "`hydra status` of this checkout as data: in a worktree, every service with where it runs (here, or main's copy), its port, URL and state, " +
				"how far behind main the branch is, and every problem with its fix; in the main checkout, main's processes and problems. Read-only.",
			schema: noArgs, run: mcpStatus,
		},
		{
			name: "up", title: "Start this worktree's stack",
			description: "Start this worktree's stack in the background (`hydra up -D`): the services the branch changed, wired to main's copies of the rest. " +
				"Returns once process-compose has the processes, or with the error that stopped it (the checks against main's stack come first). " +
				"Starting a stack that is up is not an error; with services, it also runs those here. Then call wait. Refused in the main checkout.",
			schema: object(map[string]any{
				"services": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "Also run these here, even if the branch didn't change them (`hydra up NAME`)."},
			}),
			changes: true, run: mcpUp,
		},
		{
			name: "wait", title: "Wait until processes are ready",
			description: "Wait until the named processes of this checkout's stack (all of them when none are named) are ready, healthy or finished, " +
				"or one of them fails, or the time runs out. Returns every state and the last lines of output of what failed or isn't ready. Read-only.",
			schema: object(map[string]any{
				"processes":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Process names; all of this checkout's when left out."},
				"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 3600, "description": "How long to wait (default 300)."},
			}),
			run: mcpWait,
		},
		{
			name: "down", title: "Stop this worktree's stack",
			description: "Stop this worktree's stack (`hydra down`). Main's stack and the other worktrees' are left alone. Refused in the main checkout: main's stack is the developer's.",
			schema:      noArgs, changes: true, destructive: true, run: mcpDown,
		},
		{
			name: "restart", title: "Restart one process",
			description: "Restart one process of this worktree's stack (`hydra process restart NAME`); call wait after it. Refused in the main checkout.",
			schema: object(map[string]any{
				"process": map[string]any{"type": "string", "description": "The process to restart."},
			}, "process"),
			changes: true, run: mcpRestart,
		},
		{
			name: "logs", title: "A process's output",
			description: "The last lines one process printed, optionally only those that contain a string. Reads this checkout's stack, or main's with main: true. Read-only.",
			schema: object(map[string]any{
				"process":  map[string]any{"type": "string", "description": "The process."},
				"lines":    map[string]any{"type": "integer", "minimum": 1, "maximum": 5000, "description": "How many lines (default 100)."},
				"contains": map[string]any{"type": "string", "description": "Only lines that contain this (case-sensitive), out of the last 5000."},
				"main":     map[string]any{"type": "boolean", "description": "Read the process of main's stack instead of this checkout's."},
			}, "process"),
			run: mcpLogs,
		},
		{
			name: "update_here", title: "Bring this worktree up to main",
			description: "`hydra update --here`: merge main (or the branch this worktree is stacked on) into this worktree and restart what that touched. " +
				"Main isn't pulled and its stack is left alone. It never resolves a conflict and never touches uncommitted work: it says so instead. " +
				"dry_run says what it would do. Refused in the main checkout.",
			schema: object(map[string]any{
				"dry_run": map[string]any{"type": "boolean", "description": "Only say what would happen."},
			}),
			changes: true, run: mcpUpdateHere,
		},
		{
			name: "main_status", title: "Main's stack",
			description: "The main checkout's stack, read-only: whether it is up, the commit it runs, every process's state, its problems, " +
				"and, from a worktree, which of main's services this worktree uses. Main's stack is the developer's: report what is wrong to them.",
			schema: noArgs, run: mcpMainStatus,
		},
		{
			name: "worktrees", title: "Every worktree, in the order to update them",
			description: "Main and every worktree of the repository, in the order to bring them up to date: each worktree right before the branches stacked on it. " +
				"For each: its branch, its parent (main, or the branch it is stacked on), commits behind and ahead of it, every file merging the parent in conflicts in, " +
				"uncommitted changes, what update_worktree would do (next: merge, resolve, skip or nothing), its stack (up, down, idle-stopped) and slot. " +
				"For main: how far behind its upstream it is (as of the last fetch), uncommitted changes, and whether its stack is up. Read-only.",
			schema: noArgs, run: mcpWorktrees,
		},
		{
			name: "update_main", title: "Pull main and bring its stack along",
			description: "`hydra update --main-only`: pull main (fast-forward only) and bring main's running stack to the new commit (reload, rerun the jobs and restart what changed). " +
				"No worktree is merged. It changes the developer's main checkout and stack: call it only when the developer asked for it in this conversation, " +
				"and with dry_run first, which fetches and says how far main would move and what would restart. Returns what the update printed, failures included.",
			schema: object(map[string]any{
				"dry_run": map[string]any{"type": "boolean", "description": "Only say what would happen (it fetches)."},
			}),
			changes: true, inMain: true, run: mcpUpdateMain,
		},
		{
			name: "update_worktree", title: "Bring a worktree up to its parent",
			description: "`hydra update --here`, run in the named worktree: merge its parent (main, or the branch it is stacked on) into it and restart what that touched in its stack. " +
				"Main and its stack are left alone, so it works from any checkout. It never resolves a conflict and never touches uncommitted work: it says so, " +
				"names every conflicting file and leaves the worktree as it was. Update a worktree only after the one it is stacked on (the worktrees tool's order). " +
				"dry_run says what it would do.",
			schema: object(map[string]any{
				"worktree": map[string]any{"type": "string", "description": "The worktree: its name (folder), branch or path, as the worktrees tool lists it."},
				"dry_run":  map[string]any{"type": "boolean", "description": "Only say what would happen."},
				"since": map[string]any{"type": "string", "description": "After a merge you made by hand (to resolve conflicts): the commit the worktree was at before it. " +
					"What the merge touched is then restarted in its running stack, which an update that finds nothing to merge wouldn't do."},
			}, "worktree"),
			changes: true, inMain: true, run: mcpUpdateWorktree,
		},
	}
}

// statusResult is the status tool's data.
type statusResult struct {
	Checkout string        `json:"checkout"`
	Main     bool          `json:"main"` // the server runs in the main checkout
	Worktree *snapWorktree `json:"worktree,omitempty"`
	Stack    *snapMain     `json:"mainStack,omitempty"`
}

func mcpStatus(_ context.Context, c *mcpCall) (toolResult, error) {
	os.Setenv("GIT_OPTIONAL_LOCKS", "0")
	if err := c.decode(&struct{}{}); err != nil {
		return toolResult{}, err
	}
	ms, err := loadMain(c.co)
	if err != nil {
		return toolResult{}, err
	}
	mem := processTrees()
	if c.co.IsMain() {
		m := snapMainOf(ms, mem)
		m.Subject, _ = gitOut(c.co.Root, "log", "-1", "--format=%s")
		return toolResult{summary: mainSummary(m, nil), data: statusResult{Checkout: c.co.Root, Main: true, Stack: m}}, nil
	}
	w, err := worktreeSnap(c.co, ms, mem)
	if err != nil {
		return toolResult{}, err
	}
	return toolResult{summary: worktreeSummary(w), data: statusResult{Checkout: c.co.Root, Worktree: w}}, nil
}

// worktreeSnap is this worktree as `hydra status --json` shows it.
func worktreeSnap(co gitx.Checkout, ms *mainState, mem func(int) int64) (*snapWorktree, error) {
	head, err := gitx.Head(co.Root)
	if err != nil {
		return nil, err
	}
	wt := stack.Worktree{Root: co.Root, Branch: gitx.Branch(co.Root), Head: head}
	p := planWorktree(wt, ms.Head)
	if p.err != nil {
		return nil, p.err
	}
	g := gitState(wt, ms.target.checkout.Root, ms.target.stack.Hydra.BaseRef, ms.Head, ms.target.stack.Hydra.BehindWarn)
	w := snapWorktreeOf(ms, wt, p, g, mem)
	return &w, nil
}

func worktreeSummary(w *snapWorktree) string {
	var b strings.Builder
	slot := "no slot yet"
	if w.Slot > 0 {
		slot = fmt.Sprintf("slot %d", w.Slot)
	}
	fmt.Fprintf(&b, "worktree %s (%s), %s, stack %s\n", w.Root, w.Branch, slot, upText(w.Up, "up", "not started"))
	if w.Behind > 0 {
		fmt.Fprintf(&b, "%d commit%s behind main's checkout, %d ahead\n", w.Behind, plural(w.Behind), w.Ahead)
	}
	for _, s := range w.Services {
		if s.Runs == "main" && !s.Used {
			continue
		}
		where := s.Runs
		if s.Off {
			where += " (off)"
		}
		port := ""
		if s.Port != 0 {
			port = fmt.Sprintf(" :%d", s.Port)
		}
		fmt.Fprintf(&b, "  %s  %s%s  %s\n", s.Name, where, port, s.State)
	}
	writeProblems(&b, w.Problems, "ok: everything this worktree uses is up and in step with main.")
	return strings.TrimRight(b.String(), "\n")
}

func mainSummary(m *snapMain, used []usedService) string {
	var b strings.Builder
	fmt.Fprintf(&b, "main %s on %s at %s, stack %s (%s)\n", m.Root, m.Branch, m.Commit, upText(m.Up, "up", "DOWN"), m.Where)
	if used != nil {
		if len(used) == 0 {
			b.WriteString("this worktree uses none of main's services\n")
		} else {
			b.WriteString("main's services this worktree uses:\n")
			for _, u := range used {
				fmt.Fprintf(&b, "  %s  %s  (%s)\n", u.Service, u.State, u.Why)
			}
		}
	} else {
		for _, p := range m.Processes {
			if p.Health != "off" {
				fmt.Fprintf(&b, "  %s  %s\n", p.Name, p.State)
			}
		}
	}
	writeProblems(&b, m.Problems, "ok: main's stack is up and runs what main has checked out.")
	return strings.TrimRight(b.String(), "\n")
}

func writeProblems(b *strings.Builder, ps []snapProblem, ok string) {
	if len(ps) == 0 {
		b.WriteString(ok + "\n")
		return
	}
	for _, p := range ps {
		mark := "!"
		if p.Blocks {
			mark = "✗"
		}
		fmt.Fprintf(b, "%s %s\n", mark, strings.ReplaceAll(p.Text, "\n", "\n  "))
		if p.Fix != "" {
			fmt.Fprintf(b, "  fix: %s\n", p.Fix)
		}
	}
}

type usedService struct {
	Service string `json:"service"`
	Why     string `json:"why"`
	State   string `json:"state"`
	Health  string `json:"health"`
}

type mainStatusResult struct {
	Main *snapMain `json:"main"`
	// Uses: main's services this worktree calls or waits for (from a
	// worktree only).
	Uses []usedService `json:"uses,omitempty"`
}

func mcpMainStatus(_ context.Context, c *mcpCall) (toolResult, error) {
	os.Setenv("GIT_OPTIONAL_LOCKS", "0")
	if err := c.decode(&struct{}{}); err != nil {
		return toolResult{}, err
	}
	ms, err := loadMain(c.co)
	if err != nil {
		return toolResult{}, err
	}
	m := snapMainOf(ms, processTrees())
	m.Subject, _ = gitOut(ms.target.checkout.Root, "log", "-1", "--format=%s")
	res := mainStatusResult{Main: m}
	if c.co.IsMain() {
		return toolResult{summary: mainSummary(m, nil), data: res}, nil
	}
	ws, err := loadWorktree(c.co)
	if err != nil {
		return toolResult{}, err
	}
	res.Uses = []usedService{}
	for name, why := range uses(ws.target.stack, ws.Plan) {
		u := usedService{Service: name, Why: why, State: mainProcState(ms, name), Health: "unknown"}
		if p, ok := ms.Procs[name]; ok {
			u.Health = healthName(p.Health())
		}
		res.Uses = append(res.Uses, u)
	}
	sort.Slice(res.Uses, func(i, j int) bool { return res.Uses[i].Service < res.Uses[j].Service })
	return toolResult{summary: mainSummary(m, res.Uses), data: res}, nil
}

type procState struct {
	Name   string   `json:"name"`
	State  string   `json:"state"`
	Health string   `json:"health"`
	Logs   []string `json:"logs,omitempty"` // the last lines, when it failed or isn't ready
}

func mcpUp(_ context.Context, c *mcpCall) (toolResult, error) {
	var a struct {
		Services []string `json:"services"`
	}
	if err := c.decode(&a); err != nil {
		return toolResult{}, err
	}
	for _, s := range a.Services {
		if s == "" || strings.HasPrefix(s, "-") {
			return toolResult{}, fmt.Errorf("bad service name %q", s)
		}
	}
	out, err := c.hydra(append([]string{"up", "-D"}, a.Services...)...)
	if err != nil {
		return toolResult{}, err
	}
	// process-compose -D returns before its server answers.
	cl := c.client(false)
	deadline := time.Now().Add(30 * time.Second)
	for !cl.Alive() {
		if time.Now().After(deadline) {
			return toolResult{}, fmt.Errorf("hydra up -D ended, but after 30s nothing answers on %s. What it printed:\n%s", cl.Where, out)
		}
		time.Sleep(200 * time.Millisecond)
	}
	procs, err := cl.Processes()
	if err != nil {
		return toolResult{}, err
	}
	states := make([]procState, 0, len(procs))
	for _, p := range procs {
		states = append(states, procState{Name: p.Name, State: p.Describe(), Health: healthName(p.Health())})
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Name < states[j].Name })
	summary := out
	if summary != "" {
		summary += "\n"
	}
	summary += "this worktree's stack is up; call wait for it to be ready.\n" + stateLines(states)
	return toolResult{summary: strings.TrimRight(summary, "\n"), data: map[string]any{"output": out, "processes": states}}, nil
}

func namesOf(states []procState) []string {
	var out []string
	for _, s := range states {
		out = append(out, s.Name)
	}
	return out
}

func stateLines(states []procState) string {
	var b strings.Builder
	for _, s := range states {
		fmt.Fprintf(&b, "  %s  %s\n", s.Name, s.State)
	}
	return b.String()
}

func mcpDown(_ context.Context, c *mcpCall) (toolResult, error) {
	if err := c.decode(&struct{}{}); err != nil {
		return toolResult{}, err
	}
	out, err := c.hydra("down")
	if err != nil {
		return toolResult{}, err
	}
	if out == "" {
		out = "this worktree's stack is stopped."
	}
	return toolResult{summary: out, data: map[string]any{"output": out}}, nil
}

func mcpRestart(_ context.Context, c *mcpCall) (toolResult, error) {
	var a struct {
		Process string `json:"process"`
	}
	if err := c.decode(&a); err != nil {
		return toolResult{}, err
	}
	if a.Process == "" || strings.HasPrefix(a.Process, "-") {
		return toolResult{}, fmt.Errorf("bad process name %q", a.Process)
	}
	if !c.client(false).Alive() {
		return toolResult{}, c.notUp(false)
	}
	out, err := c.hydra("process", "restart", a.Process)
	if err != nil {
		return toolResult{}, err
	}
	summary := strings.TrimSpace(out + "\nrestarted " + a.Process + "; call wait for it to be ready.")
	return toolResult{summary: summary, data: map[string]any{"process": a.Process, "output": out}}, nil
}

func mcpUpdateHere(_ context.Context, c *mcpCall) (toolResult, error) {
	var a struct {
		DryRun bool `json:"dry_run"`
	}
	if err := c.decode(&a); err != nil {
		return toolResult{}, err
	}
	args := []string{"update", "--here"}
	if a.DryRun {
		args = append(args, "--dry-run")
	}
	out, err := c.hydra(args...)
	if err != nil {
		return toolResult{}, err
	}
	return toolResult{summary: out, data: map[string]any{"output": out, "dryRun": a.DryRun}}, nil
}

// logsScanned is how far back a filter looks.
const logsScanned = 5000

func mcpLogs(_ context.Context, c *mcpCall) (toolResult, error) {
	var a struct {
		Process  string `json:"process"`
		Lines    int    `json:"lines"`
		Contains string `json:"contains"`
		Main     bool   `json:"main"`
	}
	if err := c.decode(&a); err != nil {
		return toolResult{}, err
	}
	if a.Process == "" {
		return toolResult{}, fmt.Errorf("process is required")
	}
	if a.Lines <= 0 {
		a.Lines = 100
	}
	a.Lines = min(a.Lines, logsScanned)
	cl := c.client(a.Main)
	if !cl.Alive() {
		return toolResult{}, c.notUp(a.Main)
	}
	n := a.Lines
	if a.Contains != "" {
		n = logsScanned
	}
	lines, err := cl.Logs(a.Process, n)
	if err != nil {
		return toolResult{}, fmt.Errorf("reading %s's output: %w", a.Process, err)
	}
	if a.Contains != "" {
		var hit []string
		for _, l := range lines {
			if strings.Contains(l, a.Contains) {
				hit = append(hit, l)
			}
		}
		lines = hit
		if len(lines) > a.Lines {
			lines = lines[len(lines)-a.Lines:]
		}
	}
	if lines == nil {
		lines = []string{}
	}
	head := fmt.Sprintf("%s: the last %d line%s", a.Process, len(lines), plural(len(lines)))
	if a.Contains != "" {
		head = fmt.Sprintf("%s: %d line%s with %q, of the last %d", a.Process, len(lines), plural(len(lines)), a.Contains, logsScanned)
	}
	summary := head
	if len(lines) > 0 {
		summary += ":\n" + strings.Join(lines, "\n")
	}
	return toolResult{summary: summary, data: map[string]any{"process": a.Process, "lines": lines}}, nil
}

// waitPoll is how often wait asks process-compose; a process that stays
// failed this many times in a row has failed (one between restarts looks
// failed for a moment).
var (
	waitPoll         = time.Second
	waitFailedPolls  = 3
	waitDefaultLimit = 300
)

type waitResult struct {
	Ready     bool        `json:"ready"`
	TimedOut  bool        `json:"timedOut,omitempty"`
	Waited    string      `json:"waited"`
	Processes []procState `json:"processes"`
	// NotReady: the ones that failed, are off, or were still starting when
	// the time ran out, with their last lines.
	NotReady []procState `json:"notReady"`
}

func mcpWait(ctx context.Context, c *mcpCall) (toolResult, error) {
	var a struct {
		Processes      []string `json:"processes"`
		TimeoutSeconds int      `json:"timeout_seconds"`
	}
	if err := c.decode(&a); err != nil {
		return toolResult{}, err
	}
	if a.TimeoutSeconds <= 0 {
		a.TimeoutSeconds = waitDefaultLimit
	}
	limit := time.Duration(min(a.TimeoutSeconds, 3600)) * time.Second
	cl := c.client(false)
	if !cl.Alive() {
		return toolResult{}, c.notUp(false)
	}
	// What process-compose reports, as `hydra status` corrects it (a
	// process that says it is ready with a log line).
	var refresh func() (map[string]pc.Process, error)
	if c.co.IsMain() {
		ms, err := loadMain(c.co)
		if err != nil {
			return toolResult{}, err
		}
		refresh = func() (map[string]pc.Process, error) { err := ms.refresh(); return ms.Procs, err }
	} else {
		ws, err := loadWorktree(c.co)
		if err != nil {
			return toolResult{}, err
		}
		refresh = func() (map[string]pc.Process, error) { err := ws.refresh(); return ws.Procs, err }
	}
	began := time.Now()
	bad := map[string]int{} // polls in a row a process was failed or off
	for {
		procs, err := refresh()
		if err != nil {
			return toolResult{}, err
		}
		if procs == nil {
			return toolResult{}, fmt.Errorf("the stack stopped while waiting for it (%s)", cl.Where)
		}
		names := a.Processes
		if len(names) == 0 {
			for name, p := range procs {
				if p.Health() != pc.Off {
					names = append(names, name)
				}
			}
		}
		sort.Strings(names)
		var unknown []string
		settled, done := true, false
		for _, n := range names {
			p, ok := procs[n]
			if !ok {
				unknown = append(unknown, n)
				continue
			}
			switch p.Health() {
			case pc.Up, pc.Done:
				delete(bad, n)
			case pc.Failed, pc.Off:
				bad[n]++
				settled = false
				done = done || bad[n] >= waitFailedPolls
			default:
				delete(bad, n)
				settled = false
			}
		}
		if len(unknown) > 0 {
			return toolResult{}, fmt.Errorf("no process named %s in this stack; it has: %s", strings.Join(unknown, ", "), strings.Join(sortedProcs(procs), ", "))
		}
		timedOut := time.Since(began) >= limit
		if settled || done || timedOut {
			return waitDone(cl, procs, names, settled, timedOut && !settled && !done, time.Since(began)), nil
		}
		select {
		case <-ctx.Done():
			return toolResult{}, fmt.Errorf("the wait was cancelled after %s", time.Since(began).Round(time.Second))
		case <-time.After(waitPoll):
		}
	}
}

func waitDone(cl *pc.Client, procs map[string]pc.Process, names []string, ready, timedOut bool, waited time.Duration) toolResult {
	res := waitResult{Ready: ready, TimedOut: timedOut, Waited: waited.Round(time.Second).String(), Processes: []procState{}, NotReady: []procState{}}
	for _, n := range names {
		p := procs[n]
		st := procState{Name: n, State: p.Describe(), Health: healthName(p.Health())}
		res.Processes = append(res.Processes, st)
		if h := p.Health(); h != pc.Up && h != pc.Done {
			if lines, err := cl.Logs(n, 30); err == nil {
				st.Logs = lines
			}
			res.NotReady = append(res.NotReady, st)
		}
	}
	var b strings.Builder
	switch {
	case ready:
		fmt.Fprintf(&b, "ready after %s:\n", res.Waited)
	case timedOut:
		fmt.Fprintf(&b, "not ready after %s, the time given:\n", res.Waited)
	default:
		fmt.Fprintf(&b, "not ready after %s: %s failed or stayed off:\n", res.Waited, strings.Join(namesOf(res.NotReady), ", "))
	}
	b.WriteString(stateLines(res.Processes))
	for _, st := range res.NotReady {
		if len(st.Logs) > 0 {
			fmt.Fprintf(&b, "\n%s's last output:\n", st.Name)
			for _, l := range st.Logs {
				b.WriteString("  | " + l + "\n")
			}
		}
	}
	return toolResult{summary: strings.TrimRight(b.String(), "\n"), data: res}
}
