package main

import "strings"

// MCP prompts: workflows the server hands the agent's tool, which shows
// them as commands the developer types (Claude Code: /mcp__hydra__NAME).
// They ship in the binary, so wherever `hydra mcp` is registered they are
// there, with nothing to copy into each repository, and they always match
// the tools they call.

type mcpPrompt struct {
	name, title, description string
	args                     []promptArg
	text                     string
}

type promptArg struct {
	name, description string
}

func (p *mcpPrompt) describe() map[string]any {
	args := make([]map[string]any, 0, len(p.args))
	for _, a := range p.args {
		args = append(args, map[string]any{"name": a.name, "description": a.description, "required": false})
	}
	return map[string]any{"name": p.name, "title": p.title, "description": p.description, "arguments": args}
}

// get is the prompt as prompts/get returns it: one message from the user.
// What the developer typed after the command comes last.
func (p *mcpPrompt) get(args map[string]string) map[string]any {
	text := p.text
	if extra := strings.TrimSpace(args["instructions"]); extra != "" {
		text += "\n\nThe developer added, and it wins over the steps above where they disagree:\n" + extra
	}
	return map[string]any{
		"description": p.description,
		"messages": []any{map[string]any{
			"role":    "user",
			"content": textContent(text),
		}},
	}
}

func findPrompt(name string) *mcpPrompt {
	for i := range mcpPrompts {
		if mcpPrompts[i].name == name {
			return &mcpPrompts[i]
		}
	}
	return nil
}

var mcpPrompts = []mcpPrompt{
	{
		name:  "update_all",
		title: "Update main and every worktree",
		description: "Pull main and bring its stack along, then merge it into every worktree, parents before the branches stacked on them, " +
			"resolving the conflicts hydra update leaves alone.",
		args: []promptArg{{name: "instructions", description: "Anything to do differently: worktrees to leave out, what to do with uncommitted work, ..."}},
		text: updateAllPrompt,
	},
}

const updateAllPrompt = `Bring main and every worktree of this repository up to date with hydra's MCP tools, resolving merge conflicts yourself. I asked for this: you may call update_main.

Rules for the whole run:
- Never push, rebase, reset, force anything, delete a branch or remove a worktree (except the temporary worktree you make in step 3 for a branch with no checkout). Never pass --no-verify. Never stop main's stack or run process-compose, hydra up/down or supabase commands yourself.
- Touch only what this workflow names: a worktree's (or a stack branch's) merge of its parent, and the files that merge conflicts in.
- Leave uncommitted work alone: a worktree with uncommitted changes is skipped, unless I said otherwise.

1. Plan. Call worktrees. It lists main, then every worktree in the order to update it: each worktree comes right before the branches stacked on it (marked └), so a branch is merged after its parent took in main. It also lists the branches of a PR stack that no worktree has checked out but a worktree sits on (checkout: false): main <- schema <- core <- web with only web checked out lists schema and core in their place, since web runs their code. Show me the plan as a short table in that order, one row per worktree: its name and parent, how far behind it is, and what will happen:
   - next "merge": a clean merge;
   - next "resolve": conflicts you will resolve (name the files);
   - next "skip": left alone, with the reason (uncommitted work, a merge already under way, its PR is merged or closed, ...);
   - next "nothing": already up to date.
   The conflicts are worked out against main as it is now; pulling main can add some. Ask me to confirm before you change anything, and do what I say about any row.

2. Main. Call update_main with dry_run: true. It fetches, and says how far main would move and what of main's stack would restart. If its output has a "SWITCHES ON" line, stop and ask me. Otherwise call update_main. If it fails (main has uncommitted changes, is on another branch, can't fast-forward, or a process didn't come back), stop: tell me what it printed, and change nothing else. When main moved, call worktrees again: conflicts can have changed. Tell me what is different from the plan I confirmed.

3. The worktrees, in the order worktrees gave, one at a time. A branch stacked on another is only updated once its parent is done; if its parent was skipped or failed, skip it too and say why. A branch whose PR is merged or closed (pr.state "merged" or "closed", next "skip"): never merge into it, whatever its next says after main moves; list it in the report as on a merged PR, nothing left to do. A branch stacked on one is not skipped for that: its parent's code is already in main.
   - next "nothing": move on.
   - next "skip": move on, and keep the reason for the report.
   - next "merge": call update_worktree with its name (for checkout: false, its branch: hydra merges it without a checkout). Its output says what was merged and what restarted. If it fails, keep the error for the report and move on to the next worktree that doesn't sit on this one.
   - next "resolve" on a branch with checkout: false: it needs a checkout to resolve in. Make a temporary one: git -C <main's path> worktree add <a new directory under the system's temp directory> <branch>. Resolve there exactly as below (steps a to d; skip e: nothing runs it), then remove it: git -C <main's path> worktree remove <that directory>. If you stop on it, git merge --abort there, then remove the temporary worktree all the same.
   - next "resolve" (a worktree): in that worktree's directory (its path), with git:
     a. Note the commit it is at (git rev-parse HEAD), then git merge <parent> (the parent worktrees names, nothing else). git stops on the conflicts.
     b. For each conflicted file, find what each side meant before editing it: read both versions and the merge base (git show :1:FILE, :2:FILE, :3:FILE), and the commits on each side that touched it (git log --oneline -n 10 <parent> -- FILE, and the same for HEAD). Keep both sides' intent: never just take one side to make the conflict go away. Regenerate generated files (lock files, generated types, ...) with the project's own command instead of merging their text.
     c. Run the checks the project documents for what you touched (its AGENTS.md or CLAUDE.md: type checks, linters, the tests of that area), in that worktree. Fix what the merge broke.
     d. git add the files, then git commit --no-edit: git's own merge message.
     e. Call update_worktree again with since: the commit from step a. It finds nothing left to merge, and restarts what your merge touched in that worktree's stack. Check its output.
     Stop on that worktree instead of guessing when a conflict is ambiguous: both sides changed behaviour in ways that can't both hold, database migrations or schema changes clash or need a new order, or you can't tell what one side meant. Run git merge --abort there, so the worktree is as it was, and keep the question you would ask me for the report. Then go on with the worktrees that don't sit on it.
   update_worktree runs one at a time per repository (a second one at once says another hydra update is running). Resolving conflicts in independent chains may be done in parallel, by subagents if your tool has them; within a chain, strictly in order.

4. Report. A table in the same order: worktree, then merged cleanly / resolved (the files, and the checks you ran) / skipped (why) / on a merged PR (its number) / stopped (the question for me) / failed (the error). Then what runs: whether main's stack is up and on the new commit, and which worktree stacks are up. Last, call worktrees once more and say whether anything is still behind.`
