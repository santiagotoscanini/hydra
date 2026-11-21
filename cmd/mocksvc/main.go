// Command mocksvc stands in for any service in the demo stack.
//
// It listens on $PORT and answers GET / with JSON naming itself
// ($HYDRA_SERVICE, $HYDRA_SLOT), then calls each upstream listed in
// $HYDRA_UPSTREAMS (env var names holding URLs) and nests their answers.
// Curling a frontend therefore shows the whole chain a request took.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type reply struct {
	Service   string               `json:"service"`
	Slot      string               `json:"slot"`
	Port      string               `json:"port"`
	Upstreams map[string]*upstream `json:"upstreams,omitempty"`
}

type upstream struct {
	URL   string `json:"url"`
	Reply *reply `json:"reply,omitempty"`
	Error string `json:"error,omitempty"`
}

var client = &http.Client{Timeout: 2 * time.Second}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		log.Fatal("PORT is not set")
	}
	name := os.Getenv("HYDRA_SERVICE")
	slot := os.Getenv("HYDRA_SLOT")

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		depth, _ := strconv.Atoi(r.Header.Get("X-Hydra-Depth"))
		out := reply{Service: name, Slot: slot, Port: port}
		if depth < 5 {
			for _, env := range strings.Split(os.Getenv("HYDRA_UPSTREAMS"), ",") {
				if env == "" {
					continue
				}
				if out.Upstreams == nil {
					out.Upstreams = map[string]*upstream{}
				}
				out.Upstreams[env] = call(os.Getenv(env), depth+1, r.Header.Get("X-Hydra-Worktree"))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})

	log.Printf("%s (slot %s) listening on :%s", name, slot, port)
	log.Fatal(http.ListenAndServe("127.0.0.1:"+port, nil))
}

// call asks an upstream for its reply. worktree is the tag of the request
// being served, passed on so a routed copy's calls reach that worktree.
func call(url string, depth int, worktree string) *upstream {
	u := &upstream{URL: url}
	// Mock databases get a postgres:// URL; talk HTTP to the same host:port.
	httpURL := url
	if i := strings.Index(url, "://"); i >= 0 && !strings.HasPrefix(url, "http") {
		rest := url[i+3:]
		if j := strings.Index(rest, "/"); j >= 0 {
			rest = rest[:j]
		}
		httpURL = "http://" + rest + "/"
	}
	req, _ := http.NewRequest("GET", httpURL, nil)
	req.Header.Set("X-Hydra-Depth", strconv.Itoa(depth))
	if worktree != "" {
		req.Header.Set("X-Hydra-Worktree", worktree)
	}
	resp, err := client.Do(req)
	if err != nil {
		u.Error = err.Error()
		return u
	}
	defer resp.Body.Close()
	var r reply
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		u.Error = fmt.Sprintf("bad reply: %v", err)
		return u
	}
	u.Reply = &r
	return u
}
