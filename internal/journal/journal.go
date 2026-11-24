// Package journal is hydra's record of what happened in a repository: every
// command, every problem a check found, every change in a process's state,
// and what each stack used over time. Main and all its worktrees write to
// one place, <git common dir>/hydra-journal/, one JSON object per line and
// one file per day, so a whole day of every checkout reads in order.
//
// Writing never fails a command: a journal that can't be written is skipped.
// Each event is one write(2) on a file opened with O_APPEND, so lines from the
// many hydra processes of a machine don't interleave.
package journal

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Fields are an event's data.
type Fields map[string]any

// Event is one line of the journal.
type Event struct {
	Time     time.Time `json:"t"`
	Kind     string    `json:"ev"`
	Checkout string    `json:"checkout,omitempty"` // "main", or the worktree's folder name
	Slot     *int      `json:"slot,omitempty"`
	Pid      int       `json:"pid"`
	Version  string    `json:"hydra,omitempty"`
	Fields   Fields    `json:"-"`
}

// Keep is how long a day's file is kept.
const Keep = 30 * 24 * time.Hour

// maxString caps one string in an event: a log tail is kept, a build's
// whole output isn't.
const maxString = 8 << 10

// Dir is where a repository's journal lives.
func Dir(commonDir string) string { return filepath.Join(commonDir, "hydra-journal") }

var (
	mu   sync.Mutex
	dir  string
	base Event
	off  = os.Getenv("HYDRA_JOURNAL") == "off"
)

// Open points this process's events at a repository's journal, as written
// by checkout (its slot, -1 when it has none yet) and by this version of
// hydra. It can be called again once more is known. Files older than Keep
// are deleted.
func Open(commonDir, checkout string, slot int, version string) {
	mu.Lock()
	defer mu.Unlock()
	first := dir == ""
	dir = Dir(commonDir)
	base = Event{Checkout: checkout, Pid: os.Getpid(), Version: version}
	if slot >= 0 {
		base.Slot = &slot
	}
	if first && !off {
		go prune(dir, time.Now())
	}
}

// Opened reports whether Open was called.
func Opened() bool {
	mu.Lock()
	defer mu.Unlock()
	return dir != ""
}

// SetSlot records the slot once it is known.
func SetSlot(slot int) {
	mu.Lock()
	defer mu.Unlock()
	base.Slot = &slot
}

// Log writes one event. Before Open, or with HYDRA_JOURNAL=off, it does
// nothing.
func Log(kind string, f Fields) {
	mu.Lock()
	d, e := dir, base
	mu.Unlock()
	if d == "" || off {
		return
	}
	e.Time, e.Kind, e.Fields = time.Now(), kind, f
	line, err := e.marshal()
	if err != nil {
		return
	}
	if os.MkdirAll(d, 0o755) != nil {
		return
	}
	file, err := os.OpenFile(filepath.Join(d, e.Time.Format("2006-01-02")+".jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	file.Write(line)
	file.Close()
}

func (e Event) marshal() ([]byte, error) {
	head, err := json.Marshal(struct {
		Time     string `json:"t"`
		Kind     string `json:"ev"`
		Checkout string `json:"checkout,omitempty"`
		Slot     *int   `json:"slot,omitempty"`
		Pid      int    `json:"pid"`
		Version  string `json:"hydra,omitempty"`
	}{e.Time.Format(time.RFC3339Nano), e.Kind, e.Checkout, e.Slot, e.Pid, e.Version})
	if err != nil {
		return nil, err
	}
	if len(e.Fields) == 0 {
		return append(head, '\n'), nil
	}
	body, err := json.Marshal(capStrings(e.Fields))
	if err != nil {
		return nil, err
	}
	// The fields go after the common ones, in one object.
	out := append(head[:len(head)-1], ',')
	out = append(out, body[1:]...)
	return append(out, '\n'), nil
}

func capStrings(v any) any {
	switch x := v.(type) {
	case string:
		if len(x) > maxString {
			return "…" + x[len(x)-maxString:]
		}
		return x
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = capStrings(s)
		}
		return out
	case Fields:
		return capStrings(map[string]any(x))
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if k == "t" || k == "ev" || k == "checkout" || k == "slot" || k == "pid" || k == "hydra" {
				k = "_" + k // never shadow the common fields
			}
			out[k] = capStrings(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = capStrings(val)
		}
		return out
	}
	return v
}

func prune(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		day, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok {
			continue
		}
		if t, err := time.ParseInLocation("2006-01-02", day, time.Local); err == nil && now.Sub(t) > Keep+24*time.Hour {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// Record is an event as read back: the common fields, and all of them.
type Record struct {
	Time     time.Time
	Kind     string
	Checkout string
	Slot     int // -1 when none
	Raw      map[string]any
	Line     []byte
}

// Str is a field as a string, "" when missing.
func (r Record) Str(key string) string {
	s, _ := r.Raw[key].(string)
	return s
}

// Num is a field as a number, 0 when missing.
func (r Record) Num(key string) float64 {
	n, _ := r.Raw[key].(float64)
	return n
}

// Read returns a repository's events since a time, oldest first.
func Read(commonDir string, since time.Time) ([]Record, error) {
	d := Dir(commonDir)
	entries, err := os.ReadDir(d)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	first := since.Add(-24 * time.Hour).Format("2006-01-02") // a file is named by local day
	for _, e := range entries {
		if day, ok := strings.CutSuffix(e.Name(), ".jsonl"); ok && day >= first {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var out []Record
	for _, name := range names {
		f, err := os.Open(filepath.Join(d, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			var raw map[string]any
			if json.Unmarshal(line, &raw) != nil {
				continue
			}
			t, err := time.Parse(time.RFC3339Nano, str(raw["t"]))
			if err != nil || t.Before(since) {
				continue
			}
			slot := -1
			if n, ok := raw["slot"].(float64); ok {
				slot = int(n)
			}
			out = append(out, Record{Time: t, Kind: str(raw["ev"]), Checkout: str(raw["checkout"]), Slot: slot, Raw: raw, Line: line})
		}
		f.Close()
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
