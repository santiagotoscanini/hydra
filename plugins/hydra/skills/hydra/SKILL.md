---
name: hydra
description: How to run, check and stop the app in a repository that uses hydra (a process-compose.yaml with an x-hydra block), where the main checkout runs the full dev stack and each git worktree runs only the services its branch changed. Use to start or stop the local app, find its ports and logs, when a local service doesn't answer, when `hydra up` refuses to start or `hydra status` shows a ✗, and when a branch is behind main.
---

# Running the app with hydra

hydra wraps process-compose and takes the same commands. In a repository that
uses it:

1. The **main checkout** runs the one full stack. It changes only when the
   developer runs `hydra update`.
2. A **worktree** runs what its branch changed, on ports of its own, and takes
   the rest from main's running copies.
3. `hydra status` says when either isn't true, with the command that fixes it.
   `hydra up` won't start a worktree on top of something broken.

The manual is <https://hydra.toscanini.me/docs/>; `hydra --help` lists the
commands.

## Use the MCP tools when you have them

The `hydra` MCP server (`hydra mcp`) works on the checkout your session runs
in. Prefer its tools to the shell:

| Tool | What it does |
|---|---|
| `status` | this checkout's state: what runs here, what it takes from main, every problem and its fix |
| `up`, then `wait` | start this worktree's stack in the background; wait until it is ready (failures come back with their last log lines) |
| `logs` | the last lines of one process, optionally filtered |
| `restart` | restart one process, then `wait` |
| `down` | stop this worktree's stack, and nothing else |
| `update_here` | merge main into this worktree and restart what that touched |
| `main_status` | main's stack, read-only |
| `tunnel` | the public tunnel for webhooks: `status`; `take` (its webhooks come to this worktree) and `release` (back to main) only when the developer asked |

In the main checkout, the tools that change a stack refuse: main's stack is
the developer's. Tell them what `main_status` says instead of fixing it.

Without the MCP tools, the shell commands below do the same.

## In a worktree

```bash
hydra up -D            # start what this branch changed; safe to repeat
hydra status --wait    # wait until it is up; exit 1 with the reason if not
hydra plan             # what would run here and why, without starting anything
hydra down             # stop this worktree's stack (never main's)
```

- **Ports.** Every worktree has its own (main's port + slot × stride). Read
  them from `hydra status`; never assume the default port, which is main's.
  A routed service (a frontend, say) answers on this worktree's port even
  when main's copy serves it.
- **Logs.** `hydra process logs NAME`, or `.process-compose/logs/NAME.log`
  in this worktree for what runs here and in the main checkout for the rest.
- **A service the plan left to main** that this branch needs to run itself:
  `hydra up -D NAME`. It is added to a stack that is already up.
- **Never** start services another way in a worktree (the app's own `dev`
  scripts, `docker compose up`, `process-compose` without hydra): they take
  main's ports or main's data. Never kill process-compose by hand (`pkill`
  hits every checkout's): `hydra down`.
- Stop the stack when the work is finished: it holds memory while it is up.
  A stack can also stop by itself after a while unused (`idle_stop`);
  `hydra status` says so and `hydra up -D` starts it again.

## Webhooks: the tunnel

A stack may have one public tunnel (`x-hydra.tunnel`, cloudflared or
ngrok): webhooks from Twilio, Stripe and the like arrive through it, at
main's copy of its service unless a worktree took it. `hydra tunnel` (or the
`tunnel` tool, `action: "status"`) says who holds it.

- When your task needs those webhooks in this worktree (testing a call
  flow, a payment callback), ask the developer first: taking the tunnel
  moves the webhooks away from whoever has them. Take it only when they say
  so: `hydra tunnel take`, or the tool with `action: "take"`.
- Give it back when done: `hydra tunnel release` (the tool's `release`).
  `hydra down` gives it back too.
- `hydra status` in the holder: `tunnel … comes here: api :PORT`. A ✗
  "this worktree holds tunnel …, and its api is …" means every webhook gets
  a 502: `hydra up -D api`, or release it. A `!` "webhooks through tunnel
  … go to …'s api" in a worktree that runs the api is only a note.

## When `hydra up` refuses, or `hydra status` shows a ✗

Every ✗ comes with a `fix:` line. Whether to run it yourself:

| It says | Do |
|---|---|
| this branch is behind main | `hydra update --here` (or the `update_here` tool): merges main into this branch only. On a conflict, resolve it as below. |
| main is older than this branch, or main moved without `hydra update`, or main's stack runs an older stack file | Main needs `hydra update`, which moves every worktree. Ask the developer first. |
| main's stack isn't running | Tell the developer, unless they said you may start it (`hydra up -D` in the main checkout). |
| main's `SERVICE` failed | Read its log lines and tell the developer. |
| `SERVICE` here failed, was skipped, isn't running | Yours: the log is in this worktree. Fix it, then `hydra process restart SERVICE`. |
| this worktree's stack isn't running | `hydra up -D`, then `hydra status --wait`. |
| Port N is held by a leftover of this stack | Run the `fix:` line: it stops the leftover first. |
| Port N is held by another program | Not yours to stop. Tell the developer which program and port. |
| main's copy can't serve this worktree | `hydra up -D SERVICE` runs a copy here. Say why main's couldn't. |

Notes (`!`) don't block. "changed, but in no service's paths" means nothing
runs here for those files: if a service uses them, run it here and tell the
developer the path is missing from `process-compose.yaml`.

`hydra up --force` skips the checks: only when the developer says so.

In the main checkout, don't run `hydra down`, `hydra update`, `git pull` or
`git checkout`, or restart anything nobody asked for.

## Merging main into a worktree

`hydra update --here` merges cleanly or stops. On a conflict: `git merge
main` in the worktree, never a rebase. Read both sides first (the branch's
commits and PR, and main's commits that touched each file), keep both
intents, regenerate generated files with the project's own command instead
of merging their text, run the checks the project documents, then `git
commit --no-edit`. Never `--no-verify`. If you can't tell what one side
meant: `git merge --abort` and ask. Then `hydra status`.

Updating main and every worktree is the `hydra-update-all` skill, and only
when the developer asks for it.

## When hydra misbehaves

`hydra log --here --errors` shows what went wrong in this checkout;
`hydra report` writes everything into one file. Give the developer its path
with what you were doing, instead of working around hydra.
