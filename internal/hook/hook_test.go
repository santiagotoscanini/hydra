package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// app is a Node server that knows nothing about hydra: it calls two
// addresses with fetch and with node:http and reports what came back.
const app = `
const http = require('node:http')
const viaHttp = (url) => new Promise((resolve, reject) =>
  http.get(url, (res) => { let b = ''; res.on('data', (d) => (b += d)); res.on('end', () => resolve(b)) }).on('error', reject))
http.createServer(async (req, res) => {
  const out = {
    fetch: await (await fetch(process.env.ROUTED + '/a')).text(),
    http: await viaHttp(process.env.ROUTED + '/b'),
    later: await new Promise((r) => setTimeout(async () => r(await (await fetch(process.env.ROUTED + '/c')).text()), 20)),
    other: await (await fetch(process.env.OTHER + '/d')).text(),
    queue: globalThis.__hydra.upstream('QUEUE') ?? null,
  }
  res.end(JSON.stringify(out))
}).listen(process.env.PORT, '127.0.0.1')
`

func TestHookCarriesTheTagThroughANodeService(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	// Both servers answer with the tag they saw. Only one is a routed address.
	var mu sync.Mutex
	hookSeen := map[string]string{}
	echo := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hookSeen[name+r.URL.Path] = r.Header.Get("X-Hydra-Hook")
			mu.Unlock()
			if r.URL.Path == "/__hydra/echo" {
				json.NewEncoder(w).Encode(map[string]string{"tag": r.Header.Get("X-Hydra-Worktree")})
				return
			}
			fmt.Fprintf(w, "%s saw %q", name, r.Header.Get("X-Hydra-Worktree"))
		}))
	}
	routed, other := echo("routed"), echo("other")
	defer routed.Close()
	defer other.Close()

	dir := t.TempDir()
	hookPath, err := Install(filepath.Join(dir, "with space"))
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := Install(filepath.Join(dir, "with space")); again != hookPath {
		t.Fatalf("second install moved the hook: %s", again)
	}
	appPath := filepath.Join(dir, "app.cjs")
	if err := os.WriteFile(appPath, []byte(app), 0o644); err != nil {
		t.Fatal(err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	cmd := exec.Command(node, appPath)
	cmd.Env = append(os.Environ(),
		"PORT="+strconv.Itoa(port), "ROUTED="+routed.URL, "OTHER="+other.URL,
		"HYDRA_ROUTED_ORIGINS="+routed.URL, `NODE_OPTIONS=--require "`+hookPath+`"`)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	for i := 0; ; i++ {
		if c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port)); err == nil {
			c.Close()
			break
		}
		if i > 100 {
			t.Fatal("the node app never listened")
		}
		time.Sleep(50 * time.Millisecond)
	}

	get := func(path string, headers ...string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest("GET", base+path, nil)
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("%s: %s", path, body)
		}
		return out
	}

	// Tagged: every call to the routed address carries the tag, by fetch, by
	// node:http and after a timer. A call elsewhere doesn't.
	out := get("/", "X-Hydra-Worktree", "7", "X-Hydra-Upstreams", "QUEUE=hydra%207")
	want := map[string]any{
		"fetch": `routed saw "7"`, "http": `routed saw "7"`, "later": `routed saw "7"`,
		"other": `other saw ""`, "queue": "hydra 7",
	}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("tagged %s = %v, want %v", k, out[k], v)
		}
	}
	// Untagged: nothing is added, and the app gets no per-request value.
	out = get("/")
	if out["fetch"] != `routed saw ""` || out["queue"] != nil {
		t.Errorf("untagged = %v", out)
	}
	// Two tags at once don't mix.
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(tag string) {
			defer wg.Done()
			if got := get("/", "X-Hydra-Worktree", tag)["later"]; got != `routed saw "`+tag+`"` {
				t.Errorf("tag %s: got %v", tag, got)
			}
		}(strconv.Itoa(i % 4))
	}
	wg.Wait()

	// The probe reports the tag the routed address saw, without the app.
	probe := get("/__hydra/probe", "X-Hydra-Worktree", "probe-1")
	calls, _ := probe["calls"].([]any)
	if probe["hook"] != true || len(calls) != 1 || calls[0].(map[string]any)["tag"] != "probe-1" {
		t.Errorf("probe = %v", probe)
	}
	mu.Lock()
	defer mu.Unlock()
	if hookSeen["routed/a"] != "1" || hookSeen["other/d"] != "" {
		t.Errorf("hook marker: %v", hookSeen)
	}
	if !strings.Contains(hookPath, " ") {
		t.Error("the test should install the hook under a path with a space")
	}
}
