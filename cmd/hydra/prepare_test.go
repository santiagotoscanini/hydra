package main

import (
	"fmt"
	"strings"
	"testing"
)

// fakeTemporal answers like the temporal CLI for one server.
type fakeTemporal struct {
	namespaces map[string]map[string]string // namespace -> custom attribute -> type
	described  map[string]string            // namespace -> description
	workflows  map[string]int               // namespace -> workflows left running
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
		return []byte(fmt.Sprintf(`{"namespaceInfo": {"name": %q, "description": %q}}`, ns, f.described[ns])), nil
	case "operator namespace create":
		if f.namespaces[ns] != nil {
			return nil, fmt.Errorf("Namespace already exists.")
		}
		f.namespaces[ns] = map[string]string{}
		f.describe(ns, flag("--description"))
	case "operator namespace update":
		f.describe(ns, flag("--description"))
	case "operator namespace delete":
		delete(f.namespaces, ns)
		delete(f.described, ns)
		delete(f.workflows, ns)
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

func (f *fakeTemporal) describe(ns, text string) {
	if f.described == nil {
		f.described = map[string]string{}
	}
	f.described[ns] = text
}

func TestPrepareTemporal(t *testing.T) {
	retryGap = 0
	f := &fakeTemporal{lag: 3, namespaces: map[string]map[string]string{
		"default": {"OrgID": "INDEXED_VALUE_TYPE_KEYWORD", "Tags": "INDEXED_VALUE_TYPE_KEYWORD_LIST"},
	}}
	if err := prepareTemporal(f.run, "hydra-2", "default", "72h", "/wt/a"); err != nil {
		t.Fatal(err)
	}
	got := f.namespaces["hydra-2"]
	if got["OrgID"] != "INDEXED_VALUE_TYPE_KEYWORD" || got["Tags"] != "INDEXED_VALUE_TYPE_KEYWORDLIST" || len(got) != 2 {
		t.Errorf("hydra-2 attributes = %v", got)
	}
	// Again: nothing to create.
	f.calls = nil
	if err := prepareTemporal(f.run, "hydra-2", "default", "72h", "/wt/a"); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "create") || strings.Contains(c, "delete") || strings.Contains(c, "update") {
			t.Errorf("second run changed something: %s", c)
		}
	}
	if f.described["hydra-2"] != "hydra worktree /wt/a" {
		t.Errorf("hydra-2 doesn't say whose it is: %q", f.described["hydra-2"])
	}
}

// A slot is handed on when its worktree is removed, and the namespace is
// named after the slot. The next worktree must not inherit the workflows the
// last one left running: its worker would run them with another branch's
// code.
func TestPrepareTemporalReplacesANamespaceLeftByAnotherWorktree(t *testing.T) {
	retryGap, settleAfterReplace = 0, 0
	f := &fakeTemporal{
		namespaces: map[string]map[string]string{"default": {"OrgID": "INDEXED_VALUE_TYPE_KEYWORD"}, "hydra-2": {"OrgID": "INDEXED_VALUE_TYPE_KEYWORD"}},
		described:  map[string]string{"hydra-2": "hydra worktree /wt/removed"},
		workflows:  map[string]int{"hydra-2": 3},
	}
	if err := prepareTemporal(f.run, "hydra-2", "default", "72h", "/wt/new"); err != nil {
		t.Fatal(err)
	}
	if f.workflows["hydra-2"] != 0 {
		t.Errorf("the new worktree inherited %d workflows", f.workflows["hydra-2"])
	}
	if f.described["hydra-2"] != "hydra worktree /wt/new" {
		t.Errorf("description = %q", f.described["hydra-2"])
	}
	if f.namespaces["hydra-2"]["OrgID"] == "" {
		t.Errorf("the new namespace lacks main's search attributes: %v", f.namespaces["hydra-2"])
	}

	// The same worktree again (a restart): its namespace and what runs in it
	// stay.
	f.workflows["hydra-2"] = 2
	f.calls = nil
	if err := prepareTemporal(f.run, "hydra-2", "default", "72h", "/wt/new"); err != nil {
		t.Fatal(err)
	}
	if f.workflows["hydra-2"] != 2 || strings.Contains(strings.Join(f.calls, "\n"), "delete") {
		t.Errorf("a restart replaced the worktree's own namespace: %v", f.calls)
	}

	// A namespace that doesn't say whose it is (an older hydra made it) is
	// kept and labelled, not deleted.
	f = &fakeTemporal{
		namespaces: map[string]map[string]string{"default": {}, "hydra-5": {}},
		workflows:  map[string]int{"hydra-5": 1},
	}
	if err := prepareTemporal(f.run, "hydra-5", "default", "72h", "/wt/new"); err != nil {
		t.Fatal(err)
	}
	if f.workflows["hydra-5"] != 1 || f.described["hydra-5"] != "hydra worktree /wt/new" {
		t.Errorf("unlabelled namespace: workflows %d, description %q", f.workflows["hydra-5"], f.described["hydra-5"])
	}

	// Without an owner (a file rendered by an older hydra) nothing is
	// replaced.
	f = &fakeTemporal{
		namespaces: map[string]map[string]string{"default": {}, "hydra-6": {}},
		described:  map[string]string{"hydra-6": "hydra worktree /wt/removed"},
		workflows:  map[string]int{"hydra-6": 1},
	}
	if err := prepareTemporal(f.run, "hydra-6", "default", "72h", ""); err != nil {
		t.Fatal(err)
	}
	if f.workflows["hydra-6"] != 1 {
		t.Errorf("replaced a namespace without knowing the owner")
	}
}

func TestPrepareTemporalRidesOutTimeouts(t *testing.T) {
	retryGap = 0
	// The first calls time out, as they do against a busy dev server.
	f := &fakeTemporal{timeouts: 5, namespaces: map[string]map[string]string{
		"default": {"OrgID": "INDEXED_VALUE_TYPE_KEYWORD"},
	}}
	if err := prepareTemporal(f.run, "hydra-3", "default", "72h", "/wt/a"); err != nil {
		t.Fatal(err)
	}
	if got := f.namespaces["hydra-3"]; got["OrgID"] != "INDEXED_VALUE_TYPE_KEYWORD" {
		t.Errorf("hydra-3 attributes = %v", got)
	}
	// A server that never answers still fails, with the CLI's own message.
	f = &fakeTemporal{timeouts: 1000, namespaces: map[string]map[string]string{}}
	if err := prepareTemporal(f.run, "hydra-4", "default", "72h", "/wt/a"); err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("err = %v, want the timeout", err)
	}
}
