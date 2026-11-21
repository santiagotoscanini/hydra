# Design

## Usage

`hydra` stands in for `process-compose`; arguments pass through.

- In the **main checkout** it runs everything, like plain process-compose
  (the server stays on :8099).
- In a **worktree** it runs only the services whose paths that worktree
  changed, on its own ports, wired to the baseline (or to each other). The
  worktree's process-compose server listens on its own unix socket, so
  `hydra attach`, `hydra down` and `hydra process ...` reach that worktree's
  instance and never the baseline's.
- `hydra up web` in a worktree also runs `web` there, changed or not.
- A file without an `x-hydra` block runs unchanged.
- A worktree without its own `process-compose.yaml` uses the main checkout's.

## Live changes

A worktree's project includes a `hydra-watch` process. Every 2 seconds it
re-plans the stack and, when the rendered file differs from the running one,
hands it to `process-compose project update`, which adds, removes and
restarts only the processes that changed. Hydra only handles the two
transitions:

- **unchanged → changed:** the service starts in the worktree, and services
  already there that point at it restart with the new URL.
- **changed → unchanged:** the service stops, and everything points back at
  the baseline (a stand-in takes its name if something still depends on it).

Edits to a service that is already running here are left to its own reload
(process-compose `watch`, `next dev`, `tsx watch`, ...).

Polling `git diff`/`git ls-files` (with `GIT_OPTIONAL_LOCKS=0`, so it never
takes the index lock from an agent's git) respects `.gitignore` and costs
about 0.35s per tick on the monorepo with a two-branch stack. A file watcher
on a tree with `node_modules` would cost more than it saves.

## Stacks

For `main ← b1 (db) ← b2 (backend) ← b3 (frontend)`, each branch in its own
worktree, running from b3:

- Each layer is diffed against its parent only, so b2's diff holds b2's work
  and not b1's.
- A service runs from the **topmost** layer that changed it: that worktree
  has the lower layers' commits plus its own edits. The backend runs from
  b2's worktree, so an agent's edits there reach it through its own reload.
- `hydra-watch` polls every worktree in the stack, so an edit in b1's or
  b2's worktree starts or stops services in b3's project.
- Follow services (`pnpm install`) run once per worktree that has a local
  service: `deps` for b3, `deps-b2` for b2, each in its own `node_modules`.
- Logs stay in the top worktree's `.process-compose/logs/`.

A worktree's parent, in order:

1. `parent:` in its `.hydra.local.yaml` (`main` means "not stacked").
2. The branch it was created from, per its reflog (`git worktree add -b b2
   path b1` or `git checkout -b b2 b1`), if that branch is checked out and
   still shares commits past `main`. This holds after b1 moves on.
3. The deepest checked-out branch whose tip it contains.

Branches that only share history (siblings cut from a commit nobody has
checked out) are not parents: from ancestry alone a sibling looks the same as
a parent that moved on. A branch made with `git checkout -b b2` while on b1
records "Created from HEAD"; once b1 gets new commits, b2 needs `parent: b1`.

Commands of its own: `hydra plan` (what runs where, and why), `hydra render`
(the generated file), `hydra slots` (which worktree has which slot).

## The `x-hydra` extension

Hydra reads the project's normal `process-compose.yaml`, with an `x-hydra` key
at the top level and on each process. process-compose ignores keys it doesn't
know (checked on v1.124).

```yaml
x-hydra:
  base_ref: main          # changes = diff against where HEAD left main (or its upstream) + untracked
  slot_stride: 200        # slot N listens on port + N*200 (slot 0 = baseline)
  max_slots: 20
  reserved_ports: [6379, 7233, 54321]  # shared infra; no slot may land here
  global_paths: [go.work] # affect every service that declares paths

processes:
  api:
    x-hydra:
      port: 8080                  # baseline port
      port_env: API_PORT          # env var it reads (default PORT)
      paths: ["services/core/**", "!**/*_test.go"]  # globs: **, *, {a,b}, !exclude
      upstreams:                  # env var -> service it points at
        GO_BACKEND_URL: api       # a service may point at itself
        TEMPORAL_NAMESPACE: worker
  worker:
    x-hydra:
      paths: ["services/core/**"]
      url: "hydra-{slot}"         # what upstreams receive (default http://{host}:{port})
      baseline_url: default       # ...from the baseline instance
      prepare: "temporal operator namespace create --namespace {value}"
  mobile:
    x-hydra:
      upstreams:
        API_PORT: { service: web, value: "{port}" }  # just the port
  deps:
    x-hydra: { follow: true }     # runs here whenever a local service depends on it
  supabase:
    x-hydra: { shared: true, paths: ["supabase/migrations/**"] }  # always baseline; warns on change
```

A process without `x-hydra` is treated as shared: it only runs in the
baseline.

## What a worktree runs

1. Services whose `paths` match a changed file (or `global_paths`, for
   services that declare paths).
2. Services named with `--with`, `hydra up NAME` or `.hydra.local.yaml`.
3. Services with a route override, plus the target of an `=local` override.
4. `follow` services that a local service depends on.
5. Minus `shared` services and anything in `--without`.

## Routing

An upstream points at the worktree's instance if that service runs in the
worktree, else at the baseline. Override per service and upstream with
`--route web.api=baseline|local` or in the worktree's git-ignored
`.hydra.local.yaml`:

```yaml
with: [web]
without: [worker]
routes:
  web: { api: baseline }
```

Upstream values aren't only URLs. A Temporal namespace or a LiveKit instance
ID is "where the service lives" too, so the same mechanism keeps a worktree's
jobs on its own worker and a worktree's calls on its own agent.

## Rendering

`hydra render` writes `.process-compose/hydra.yaml` in the checkout:

- Only the local services, with `x-hydra` removed.
- `PORT` (or `port_env`), each upstream's env var, `HYDRA_SLOT`,
  `HYDRA_SERVICE` added to `environment`. Process env beats `.env` files in
  Next.js and godotenv, so the injected values win.
- `working_dir`, `log_location` and `watch` paths made absolute
  (process-compose resolves some against the config file's directory).
- Readiness and liveness probes on the base port moved to the slot port.
  Without this, a worktree's api probe hits the baseline's and passes at once.
- A **stand-in** for every baseline process a local one depends on, with the
  same name, in the `baseline` namespace. It runs nothing and copies only the
  readiness probe (or ready log line). `process_healthy` therefore waits for
  the baseline's Supabase instead of starting a second one, and stopping the
  worktree never runs the baseline's shutdown commands.
- A `hydra-prepare-<service>` step before each service with `prepare`.

## Slots

`hydra-slots.json` in the shared `.git` directory maps worktree paths to
slots, guarded by flock. A worktree keeps its slot across runs, and entries
for deleted worktrees are dropped. Ports for every slot up to `max_slots` are
checked against each other and `reserved_ports` when the config loads.

## Open problems

- **Migrations.** Most branches that touch the backend add a migration, and
  the database is shared. Options: apply the worktree's migrations to the
  shared database (fine for additive changes, breaks when two branches
  conflict), a per-worktree database in the same Postgres (works for services
  that connect directly, not for ones that go through Supabase's REST/auth),
  or a second Supabase stack on shifted ports only for worktrees that change
  migrations.
- **Scheduled work.** A worktree worker in its own Temporal namespace creates
  that namespace's schedules on startup, so cron-like jobs run twice against
  the shared database. Needs a way to skip schedule setup in worktree slots.
- **Dependency paths by hand.** `paths` lists workspace packages manually.
  They could be derived from the nx project graph or `go list -deps`. Go
  package-level deps would also tell api changes apart from worker changes.
- **Hard-coded ports** in app code or dev scripts need a small patch each
  (or a command override in the process-compose file).
- **Routing to another worktree** (`web.api=slot:3`), and hostnames instead of
  ports (`api.slot1.localhost`), so cookies and CORS behave as on the
  baseline.
- **PR bases as a parent source.** Stacked PRs already record the parent
  branch on GitHub; reading it would remove most `parent:` overrides.
