---
name: hydra-setup
description: Set up hydra in a repository - write the x-hydra blocks in its process-compose.yaml (or a first process-compose.yaml) so the main checkout runs the full dev stack and each git worktree runs only the services its branch changed. Use when the developer asks to adopt, add, configure or set up hydra, to make the dev stack work across worktrees or agents in parallel, or to fix which services a worktree runs (paths, ports, upstreams).
---

# Setting up hydra in a repository

hydra reads the `process-compose.yaml` the repository already has, plus
`x-hydra` keys that process-compose itself ignores. The goal: every
service a worktree may run says which files belong to it (`paths`), where
it listens (`port`, `port_env`) and where it finds the services it calls
(`upstreams`), so hydra can run that service alone in a worktree, on ports
of its own, wired to the main checkout's copies of the rest.

The manual, with every key: <https://hydra.toscanini.me/docs/#quickstart>
and <https://hydra.toscanini.me/docs/#config>.

## 1. Learn the stack first

Before writing anything, find out, for each process:

- **What it is.** A database, a broker or a tunnel holds data or is
  expensive: `shared`, main only. An API, a worker or a frontend is code a
  branch changes: worktrees run their own. A dependency step (`pnpm
  install`) runs where something needs it: `follow`.
- **Which files are its code.** Its source directory, the shared packages
  it imports, its migrations. Read the build files (`go.mod`, workspace
  `package.json`s, `nx.json`, `turbo.json`) to see what imports what. A
  file missing from `paths` means a change there runs nothing in the
  worktree, silently; `ignore` lists files that belong to no service (docs,
  CI) so that hydra stops reporting them.
- **How it reads its port and the addresses of what it calls.** hydra
  sets environment variables; the service must read them, not hard-code a
  port or a URL. Find the variable names in its config or `.env` files.
  If a service hard-codes them, say so: making it read a variable is a code
  change for the developer to approve.
- **Who calls it**, not only what it calls: a frontend calls the API, and
  a worker may call it back. Each caller needs an `upstreams` entry.

If there is no `process-compose.yaml` yet, write one from how the
developer starts the app today (`package.json` scripts, a Procfile,
`docker-compose.yml`, the README), and check with them before replacing
their way of starting it.

## 2. Write the x-hydra blocks

```yaml
version: "0.5"

x-hydra:
  slot_stride: 200          # slot N listens on every port + N × 200
  global_paths: [pnpm-lock.yaml]
  ignore: ["docs/**", ".github/**", "**/*.md"]

processes:
  db:
    command: docker compose up postgres
    x-hydra:
      shared: true          # only main's copy, ever
      paths: ["db/migrations/**"]

  deps:
    command: pnpm install
    x-hydra:
      follow: true          # runs in a worktree when a service there needs it

  api:
    command: go run ./services/api
    x-hydra:
      port: 8080
      port_env: API_PORT
      paths: ["services/api/**", "packages/shared/**", "!**/*_test.go"]

  web:
    command: pnpm --filter web dev
    depends_on:
      deps: { condition: process_completed_successfully }
    x-hydra:
      port: 3000
      paths: ["apps/web/**", "packages/shared/**"]
      routed: node          # main's copy serves worktrees that didn't change web
      upstreams:
        API_URL: api        # this worktree's api if it runs here, main's otherwise
```

Rules that keep it right:

- Pick `slot_stride` so no service's port + N × stride lands on another
  service's port, for every slot up to `max_slots`; list ports used outside
  the stack in `reserved_ports`. hydra refuses a file whose ports collide,
  and `hydra plan` says which.
- A port near 49152 or above (Supabase's 54321): set `worktree_port` lower,
  so worktree copies stay out of the range macOS uses for outgoing
  connections.
- `routed` only for HTTP services that are expensive to run per worktree
  (a Next.js dev server). `node` loads a script that passes the worktree's
  header along inside the Node process; anything else must forward the
  `X-Hydra-Worktree` header itself (`routed: true`).
- Upstream values that aren't URLs (a queue name, a database URL) can't be
  routed per request; give them `url` with `{slot}` (`jobs-{slot}`) and a
  `baseline_url` for main.
- A local Supabase: `kind: supabase`; a Temporal worker: `kind: temporal`.
  Read the manual's sections on both before using them.
- Don't add `x-hydra` to a process only main should run: without a block,
  a process is shared.
- A public tunnel for webhooks (cloudflared, ngrok) that worktrees should be
  able to take: `x-hydra: { tunnel: { to: api, port: 8079, url:
  https://hooks.example.com } }` on its process (it stays shared). `to` is
  the service its requests reach, `port` a free port where hydra's router
  listens. Tell the developer the one step hydra can't do: point the
  tunnel at `http://localhost:PORT` (for cloudflared, the public hostname's
  service in Cloudflare's dashboard) instead of the service's own port.

## 3. Ignore hydra's files

Add `.process-compose/` and `.hydra.local.yaml` to `.gitignore` (or to
`.git/info/exclude` if the developer would rather not commit that).

## 4. Check it without starting anything

```bash
hydra plan                                  # in the main checkout: the full stack
git worktree add ../<repo>-try -b hydra-try # a throwaway worktree
cd ../<repo>-try && touch services/api/x.go && hydra plan   # api runs here, the rest is main's
hydra render                                # the generated process-compose file
```

For each service, change one of its files in the throwaway worktree and
check that `hydra plan` runs that service (and what depends on it through a
value that can't be routed) and nothing else; check that each `upstreams`
variable points where it should. Then remove the throwaway worktree with
the developer's tool, or `git worktree remove`.

## 5. Hand it over

Tell the developer, in this order:

- **Before the first start:** in the main checkout, `export
  PC_PORT_NUM=8099` (or any free port) in their shell profile when a
  service of theirs listens on 8080, process-compose's default API port.
  Turn off anything that pulls the default branch by itself (editor or
  agent-app auto-pull), and have new worktrees start from local `main`.
- **Start:** `hydra up -D` in the main checkout, then `hydra status --wait`.
  Then `hydra up -D` in a worktree. Bare `hydra` opens the dashboard.
- **For agents:** the hydra plugin (or `hydra mcp` registered with their
  tool) gives agents the commands as tools that can't touch main's stack.
  One line in `AGENTS.md`: "Run the app only through the hydra MCP tools
  (status, up, wait, logs, restart, down)."
- What you couldn't decide (a service that hard-codes its port, a path you
  weren't sure of).

Never start main's stack yourself while setting up unless the developer
asks: it takes the usual ports, which may be in use by their current way of
running the app.
