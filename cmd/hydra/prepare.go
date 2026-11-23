package main

import (
	"encoding/json"
	"flag"
	"fmt"

	"os/exec"
	"sort"
	"strings"
	"time"
)

// prepareCmd runs the step a kind adds to its service in a worktree (see
// prepareSupabase for supabase's, which runs after the start). `hydra prepare temporal --namespace hydra-2 --from default`
// creates the namespace if needed and gives it every custom search attribute
// the baseline's namespace has, so workflows that set them start the same
// way. Safe to run again. The temporal CLI reads TEMPORAL_ADDRESS (default
// 127.0.0.1:7233).
func prepareCmd(args []string) error {
	if len(args) > 0 && args[0] == "supabase" {
		return prepareSupabase(args[1:])
	}
	if len(args) == 0 || args[0] != "temporal" {
		return fmt.Errorf("usage: hydra prepare temporal --namespace NAME --from NAME | hydra prepare supabase")
	}
	fs := flag.NewFlagSet("prepare temporal", flag.ContinueOnError)
	ns := fs.String("namespace", "", "")
	from := fs.String("from", "default", "")
	retention := fs.String("retention", "72h", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *ns == "" {
		return fmt.Errorf("prepare temporal: --namespace is required")
	}
	return prepareTemporal(temporalCLI, *ns, *from, *retention)
}

// temporalCLI runs the temporal CLI and returns its stdout.
func temporalCLI(args ...string) ([]byte, error) {
	out, err := exec.Command("temporal", args...).Output()
	if ee, ok := err.(*exec.ExitError); ok {
		return out, fmt.Errorf("temporal %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
	}
	return out, err
}

func prepareTemporal(run func(...string) ([]byte, error), ns, from, retention string) error {
	// Any call can time out while the server is busy, and the first calls
	// that name a new namespace often do: the CLI checks the connection
	// against that namespace before every command. Try those again.
	cli := func(args ...string) (out []byte, err error) {
		for i := 0; i < 20; i++ {
			if out, err = run(args...); err == nil || !transient(err) {
				return out, err
			}
			time.Sleep(retryGap)
		}
		return out, err
	}
	if _, err := cli("operator", "namespace", "describe", "--namespace", ns); err != nil {
		fmt.Printf("creating Temporal namespace %s\n", ns)
		if _, err := cli("operator", "namespace", "create", "--namespace", ns, "--retention", retention); err != nil {
			return err
		}
		// A new namespace takes a moment to be usable everywhere.
		for i := 0; ; i++ {
			if _, err := cli("operator", "namespace", "describe", "--namespace", ns); err == nil {
				break
			}
			if i > 60 {
				return fmt.Errorf("namespace %s was created but never became visible", ns)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	want, err := customAttributes(cli, from)
	if err != nil {
		return err
	}
	// A new namespace answers `describe` a few seconds before the search
	// attribute service knows it.
	var have map[string]string
	if err := retry(func() (err error) { have, err = customAttributes(cli, ns); return err }); err != nil {
		return err
	}
	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := have[name]; ok {
			continue
		}
		typ, ok := attributeTypes[want[name]]
		if !ok {
			return fmt.Errorf("search attribute %s in %s has a type hydra doesn't know: %s", name, from, want[name])
		}
		fmt.Printf("adding search attribute %s (%s) to %s\n", name, typ, ns)
		if err := retry(func() error {
			_, err := cli("operator", "search-attribute", "create", "--namespace", ns, "--name", name, "--type", typ)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// retry gives a namespace that was just created time to show up: Temporal
// answers "not found" until then.
func retry(fn func() error) error {
	var err error
	for i := 0; i < 60; i++ {
		if err = fn(); err == nil || !strings.Contains(err.Error(), "not found") {
			return err
		}
		time.Sleep(retryGap)
	}
	return err
}

var retryGap = 500 * time.Millisecond

// transient reports whether a temporal CLI error is the server not
// answering in time, rather than an answer.
func transient(err error) bool {
	msg := err.Error()
	for _, s := range []string{"deadline exceeded", "failed connecting", "Unavailable", "connection refused"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

var attributeTypes = map[string]string{
	"INDEXED_VALUE_TYPE_KEYWORD":      "Keyword",
	"INDEXED_VALUE_TYPE_KEYWORD_LIST": "KeywordList",
	"INDEXED_VALUE_TYPE_TEXT":         "Text",
	"INDEXED_VALUE_TYPE_INT":          "Int",
	"INDEXED_VALUE_TYPE_DOUBLE":       "Double",
	"INDEXED_VALUE_TYPE_BOOL":         "Bool",
	"INDEXED_VALUE_TYPE_DATETIME":     "Datetime",
}

func customAttributes(cli func(...string) ([]byte, error), ns string) (map[string]string, error) {
	out, err := cli("operator", "search-attribute", "list", "--namespace", ns, "-o", "json")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Custom map[string]string `json:"customAttributes"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("reading the search attributes of %s: %w", ns, err)
	}
	if doc.Custom == nil {
		doc.Custom = map[string]string{}
	}
	return doc.Custom, nil
}
