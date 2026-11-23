package main

import (
	"fmt"
	"strings"
	"testing"
)

// fakeTemporal answers like the temporal CLI for one server.
type fakeTemporal struct {
	namespaces map[string]map[string]string // namespace -> custom attribute -> type
	calls      []string
	lag        int // how many search-attribute calls on a new namespace say "not found"
	timeouts   int // how many calls time out before the server answers
}

func (f *fakeTemporal) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if f.timeouts > 0 {
		f.timeouts--
		return nil, fmt.Errorf("temporal %s: Error: failed connecting to Temporal server at localhost:7233: context deadline exceeded", strings.Join(args, " "))
	}
	flag := func(name string) string {
		for i, a := range args {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	ns := flag("--namespace")
	switch strings.Join(args[:3], " ") {
	case "operator namespace describe":
		if f.namespaces[ns] == nil {
			return nil, fmt.Errorf("namespace %s not found", ns)
		}
	case "operator namespace create":
		f.namespaces[ns] = map[string]string{}
	case "operator search-attribute list", "operator search-attribute create":
		if ns != "default" && f.lag > 0 {
			f.lag--
			return nil, fmt.Errorf("Namespace %s is not found.", ns)
		}
	}
	switch strings.Join(args[:3], " ") {
	case "operator search-attribute list":
		out := []string{}
		for k, v := range f.namespaces[ns] {
			out = append(out, fmt.Sprintf("%q: %q", k, v))
		}
		return []byte(`{"customAttributes": {` + strings.Join(out, ",") + `}, "systemAttributes": {"WorkflowId": "INDEXED_VALUE_TYPE_KEYWORD"}}`), nil
	case "operator search-attribute create":
		f.namespaces[ns][flag("--name")] = "INDEXED_VALUE_TYPE_" + strings.ToUpper(flag("--type"))
	}
	return nil, nil
}

func TestPrepareTemporal(t *testing.T) {
	retryGap = 0
	f := &fakeTemporal{lag: 3, namespaces: map[string]map[string]string{
		"default": {"OrgID": "INDEXED_VALUE_TYPE_KEYWORD", "Tags": "INDEXED_VALUE_TYPE_KEYWORD_LIST"},
	}}
	if err := prepareTemporal(f.run, "hydra-2", "default", "72h"); err != nil {
		t.Fatal(err)
	}
	got := f.namespaces["hydra-2"]
	if got["OrgID"] != "INDEXED_VALUE_TYPE_KEYWORD" || got["Tags"] != "INDEXED_VALUE_TYPE_KEYWORDLIST" || len(got) != 2 {
		t.Errorf("hydra-2 attributes = %v", got)
	}
	// Again: nothing to create.
	f.calls = nil
	if err := prepareTemporal(f.run, "hydra-2", "default", "72h"); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "create") {
			t.Errorf("second run created something: %s", c)
		}
	}
}

func TestPrepareTemporalRidesOutTimeouts(t *testing.T) {
	retryGap = 0
	// The first calls time out, as they do against a busy dev server.
	f := &fakeTemporal{timeouts: 5, namespaces: map[string]map[string]string{
		"default": {"OrgID": "INDEXED_VALUE_TYPE_KEYWORD"},
	}}
	if err := prepareTemporal(f.run, "hydra-3", "default", "72h"); err != nil {
		t.Fatal(err)
	}
	if got := f.namespaces["hydra-3"]; got["OrgID"] != "INDEXED_VALUE_TYPE_KEYWORD" {
		t.Errorf("hydra-3 attributes = %v", got)
	}
	// A server that never answers still fails, with the CLI's own message.
	f = &fakeTemporal{timeouts: 1000, namespaces: map[string]map[string]string{}}
	if err := prepareTemporal(f.run, "hydra-4", "default", "72h"); err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("err = %v, want the timeout", err)
	}
}
