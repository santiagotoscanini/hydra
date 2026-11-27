package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The agent plugin in plugins/hydra ships skills next to the MCP server.
// The update-all skill is the update_all prompt for agents whose tool has
// skills but no MCP prompts: it is written from the prompt, so the two
// can't drift. HYDRA_WRITE_SKILLS=1 go test -run TestPlugin ./cmd/hydra
// writes it again after the prompt changed.

const pluginDir = "../../plugins/hydra"

func updateAllSkill() string {
	return `---
name: hydra-update-all
description: Bring main and every git worktree of a hydra repository up to date - pull main, bring its running stack along, then merge it into each worktree and stacked branch in order, parents first, resolving the merge conflicts that hydra update leaves alone. Only when the developer asks to update, refresh or sync main and the worktrees. Needs the hydra MCP server.
disable-model-invocation: true
---

# Update main and every worktree

This is the ` + "`update_all`" + ` prompt of ` + "`hydra mcp`" + `, for agents whose tool has skills
but doesn't show MCP prompts. It needs the hydra MCP tools (` + "`worktrees`" + `,
` + "`update_main`" + `, ` + "`update_worktree`" + `); without them, stop and say that the hydra
MCP server isn't registered (` + "`hydra mcp`" + `, see
<https://hydra.toscanini.me/docs/#mcp>). Anything the developer said along
with the request wins over the steps below where they disagree.

` + updateAllPrompt + "\n"
}

func TestPluginUpdateAllSkillIsThePrompt(t *testing.T) {
	path := filepath.Join(pluginDir, "skills", "hydra-update-all", "SKILL.md")
	want := updateAllSkill()
	if os.Getenv("HYDRA_WRITE_SKILLS") == "1" {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s isn't the update_all prompt: run HYDRA_WRITE_SKILLS=1 go test -run TestPlugin ./cmd/hydra and commit it", path)
	}
}

// Every skill has the frontmatter agents read, named after its folder, and
// the MCP tools it names exist.
func TestPluginSkills(t *testing.T) {
	tools := map[string]bool{}
	for _, tl := range mcpTools {
		tools[tl.name] = true
	}
	dirs, err := os.ReadDir(filepath.Join(pluginDir, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) == 0 {
		t.Fatal("no skills")
	}
	front := regexp.MustCompile(`(?s)\A---\nname: ([a-z0-9-]+)\ndescription: ([^\n]+)\n(?:[a-z-]+: [^\n]+\n)*---\n`)
	table := regexp.MustCompile("(?m)^\\| `([a-z_]+)`")
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(pluginDir, "skills", d.Name(), "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		m := front.FindSubmatch(b)
		if m == nil {
			t.Errorf("%s: no frontmatter with name and description", d.Name())
			continue
		}
		if string(m[1]) != d.Name() {
			t.Errorf("%s: named %q", d.Name(), m[1])
		}
		if n := len(m[2]); n > 1024 {
			t.Errorf("%s: description is %d characters; agents take 1024", d.Name(), n)
		}
		for _, tm := range table.FindAllSubmatch(b, -1) {
			if !tools[string(tm[1])] && !strings.Contains(string(tm[1]), " ") {
				t.Errorf("%s: names a tool hydra mcp doesn't have: %s", d.Name(), tm[1])
			}
		}
	}
}

// The manifests parse, and say the same name and version.
func TestPluginManifests(t *testing.T) {
	read := func(p string) map[string]any {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		dec := json.NewDecoder(bytes.NewReader(b))
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		return v
	}
	plugin := read(filepath.Join(pluginDir, ".claude-plugin", "plugin.json"))
	market := read("../../.claude-plugin/marketplace.json")
	if plugin["name"] != "hydra" {
		t.Errorf("plugin.json: name %v", plugin["name"])
	}
	entries, _ := market["plugins"].([]any)
	if len(entries) != 1 {
		t.Fatalf("marketplace.json: %d plugins", len(entries))
	}
	e := entries[0].(map[string]any)
	if e["name"] != plugin["name"] || e["source"] != "./plugins/hydra" {
		t.Errorf("marketplace.json: %v", e)
	}
	servers, _ := plugin["mcpServers"].(map[string]any)
	h, _ := servers["hydra"].(map[string]any)
	if h["command"] != "hydra" {
		t.Errorf("plugin.json: the MCP server is %v", servers)
	}
}
