package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
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
		return fmt.Errorf("usage: hydra prepare temporal --namespace NAME --from NAME | hydra prepare supabase ...")
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
	// HYDRA_ROOT is the worktree that holds the slot (set on the rendered
	// step): the namespace is named after the slot, and belongs to whoever
	// holds it now.
	return prepareTemporal(temporalCLI, *ns, *from, *retention, os.Getenv("HYDRA_ROOT"))
}

// ownerMark starts the description hydra gives a namespace it creates,
// followed by the worktree it was created for.
const ownerMark = "hydra worktree "

// settleAfterReplace is how long a namespace that replaced another of the
// same name is left alone: Temporal's services cache which namespace a name
// stands for, and until that is refreshed a call by name can still reach the
// one that was deleted.
var settleAfterReplace = 5 * time.Second

// temporalCLI runs the temporal CLI and returns its stdout.
func temporalCLI(args ...string) ([]byte, error) {
	out, err := exec.Command("temporal", args...).Output()
	if ee, ok := err.(*exec.ExitError); ok {
		return out, fmt.Errorf("temporal %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
	}
	return out, err
}

// prepareTemporal makes sure the namespace exists, belongs to owner (the
// worktree that holds the slot it is named after) and has the search
// attributes of from.
//
// Slots are handed on: when a worktree is removed, the next one gets its
// slot and with it the namespace's name. Workflows the old worktree left
// running there would be picked up by the new worktree's worker and run with
// another branch's code. So a namespace created for another worktree is
// deleted and made again, empty. owner "" (an older rendered file) skips
// this.
func prepareTemporal(run func(...string) ([]byte, error), ns, from, retention, owner string) error {
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
	create := []string{"operator", "namespace", "create", "--namespace", ns, "--retention", retention}
	if owner != "" {
		create = append(create, "--description", ownerMark+owner)
	}
	desc, err := cli("operator", "namespace", "describe", "--namespace", ns, "-o", "json")
	replaced := false
	if was, mine := namespaceOwner(desc); err == nil && owner != "" && !mine(owner) {
		if was == "" {
			// Made by an older hydra, or by hand: nothing says whose it was.
			// Keep it, and put this worktree's name on it.
			if _, err := cli("operator", "namespace", "update", "--namespace", ns, "--description", ownerMark+owner); err != nil {
				return err
			}
		} else {
			fmt.Printf("Temporal namespace %s was made for %s, which held this slot before; replacing it, so what that worktree left running doesn't run here\n", ns, was)
			if _, err := cli("operator", "namespace", "delete", "--namespace", ns, "--yes"); err != nil {
				return err
			}
			err, replaced = errors.New("deleted"), true
		}
	}
	if err != nil {
		fmt.Printf("creating Temporal namespace %s\n", ns)
		if _, err := retryWhile("already exists", func() ([]byte, error) { return cli(create...) }); err != nil {
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
		if replaced {
			time.Sleep(settleAfterReplace)
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

// namespaceOwner reads `namespace describe -o json`: the worktree the
// namespace was made for ("" when its description doesn't say), and a
// function that tells whether that is the given one.
func namespaceOwner(describe []byte) (string, func(string) bool) {
	var doc struct {
		Info struct {
			Description string `json:"description"`
		} `json:"namespaceInfo"`
	}
	json.Unmarshal(describe, &doc)
	was, ok := strings.CutPrefix(doc.Info.Description, ownerMark)
	if !ok {
		was = ""
	}
	return was, func(owner string) bool { return was == owner }
}

// retryWhile runs fn again while its error mentions what: a namespace that
// was just deleted keeps its name for a moment.
func retryWhile(what string, fn func() ([]byte, error)) (out []byte, err error) {
	for i := 0; i < 40; i++ {
		if out, err = fn(); err == nil || !strings.Contains(strings.ToLower(err.Error()), what) {
			return out, err
		}
		time.Sleep(retryGap)
	}
	return out, err
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
