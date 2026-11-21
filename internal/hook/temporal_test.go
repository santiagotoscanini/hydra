package hook

import (
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A stand-in for @temporalio/client with the same shape as 1.17: every
// client extends BaseClient, keeps its namespace in this.options, and Client
// builds its sub-clients through the other modules' exports.
var fakeSDK = map[string]string{
	"package.json":       `{"name":"@temporalio/client","main":"lib/index.js"}`,
	"lib/base-client.js": `class BaseClient { constructor(o) { this.connection = o && o.connection } withDeadline() {} }; exports.BaseClient = BaseClient`,
	"lib/workflow-client.js": `const b = require('./base-client')
class WorkflowClient extends b.BaseClient { options; constructor(o) { super(o); this.options = { namespace: 'default', ...o } }
  start() { return { namespace: this.options.namespace } } }
exports.WorkflowClient = WorkflowClient`,
	"lib/schedule-client.js": `const b = require('./base-client')
class ScheduleClient extends b.BaseClient { options; constructor(o) { super(o); this.options = { namespace: 'default', ...o } } }
exports.ScheduleClient = ScheduleClient`,
	"lib/client.js": `const b = require('./base-client'), w = require('./workflow-client'), s = require('./schedule-client')
class Client extends b.BaseClient { options; constructor(o = {}) { super(o)
  this.workflow = new w.WorkflowClient({ ...o }); this.schedule = new s.ScheduleClient({ ...o })
  this.options = { namespace: 'default', ...o } } }
exports.Client = Client`,
	"lib/index.js": `for (const m of ['./client', './workflow-client', './schedule-client']) {
  const mod = require(m); for (const k of Object.keys(mod)) Object.defineProperty(exports, k, { enumerable: true, get: () => mod[k] }) }`,
}

// The app makes one client at startup, like apps/web/lib/temporal.ts.
const temporalApp = `
const http = require('node:http')
const { Client } = require('@temporalio/client')
const client = new Client({ namespace: process.env.TEMPORAL_NAMESPACE })
http.createServer((req, res) => {
  if (req.url === '/later') return setTimeout(() => res.end(client.workflow.start().namespace), 20)
  res.end(client.workflow.start().namespace)
}).listen(process.env.PORT, '127.0.0.1')
`

func TestHookSwitchesTheTemporalNamespace(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	dir := t.TempDir()
	for name, src := range fakeSDK {
		p := filepath.Join(dir, "node_modules", "@temporalio", "client", name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, "app.cjs"), []byte(temporalApp), 0o644)
	hookPath, err := Install(filepath.Join(dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	cmd := exec.Command(node, "app.cjs")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PORT="+strconv.Itoa(port), "TEMPORAL_NAMESPACE=default",
		"HYDRA_PER_REQUEST=TEMPORAL_NAMESPACE", "NODE_OPTIONS=--require "+hookPath)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	get := func(path string, upstreams string) string {
		t.Helper()
		req, _ := http.NewRequest("GET", base+path, nil)
		if upstreams != "" {
			req.Header.Set("X-Hydra-Worktree", "3")
			req.Header.Set("X-Hydra-Upstreams", upstreams)
		}
		for i := 0; ; i++ {
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				return strings.TrimSpace(string(b))
			}
			if i > 100 {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	if got := get("/", "TEMPORAL_NAMESPACE=hydra-3"); got != "hydra-3" {
		t.Errorf("worktree request started in %q, want hydra-3", got)
	}
	if got := get("/later", "TEMPORAL_NAMESPACE=hydra-3"); got != "hydra-3" {
		t.Errorf("after a timer: %q, want hydra-3", got)
	}
	if got := get("/", ""); got != "default" {
		t.Errorf("untagged request started in %q, want default", got)
	}
	// A value for another variable doesn't touch the namespace.
	if got := get("/", "QUEUE=jobs-3"); got != "default" {
		t.Errorf("unrelated value: %q, want default", got)
	}
	if probe := get("/__hydra/probe", "TEMPORAL_NAMESPACE=x"); !strings.Contains(probe, `"temporal":"ok"`) {
		t.Errorf("probe = %s", probe)
	}
}
