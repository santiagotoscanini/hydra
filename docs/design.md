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
  base_ref: develop       # optional: the branch worktrees are diffed against (default: detected, see below)
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
      kind: temporal              # a namespace per worktree: url, baseline_url and prepare filled in (see below)
  mobile:
    x-hydra:
      upstreams:
        API_PORT: { service: web, value: "{port}" }  # just the port
  web:
    x-hydra:
      port: 3000
      routed: node                # main's copy serves every worktree that didn't change it (see below)
      upstreams:
        GO_BACKEND_URL: api
        TEMPORAL_NAMESPACE: worker  # switched per request on a routed service
  deps:
    x-hydra: { follow: true }     # runs here whenever a local service depends on it
  supabase:
    x-hydra: { shared: true, paths: ["supabase/migrations/**"] }  # always baseline; warns on change
```

A process without `x-hydra` is treated as shared: it only runs in the
baseline.

Without `base_ref`, worktrees are diffed against the repository's default
branch: the one `origin` calls its default (`git symbolic-ref
refs/remotes/origin/HEAD`, recorded by `git clone`), else a local `main` or
`master`, else the branch the main checkout is on. The changed files are the
diff against where HEAD left that branch (or its upstream), plus untracked
files.

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

## Kinds

`kind` names a service Hydra knows more about than its port. It fills in the
keys you'd otherwise write by hand; any key you set yourself wins.

| Kind | Fills in | Also |
|---|---|---|
| `temporal` | `url: hydra-{slot}`, `baseline_url: default`, and a prepare step `hydra prepare temporal --namespace hydra-N --from default` | Routed services switch upstreams pointing at it per request (`per_request` is implied) |

`hydra prepare temporal` creates the namespace if needed, waits until
Temporal knows it, and adds every custom search attribute the baseline's
namespace has. It is safe to run again and reads `TEMPORAL_ADDRESS` like the
temporal CLI. A kind can't be `shared`, `follow` or `routed`: its point is a
separate copy per worktree.

New kinds (SQS queues, Redis key prefixes, a database per worktree) go in the
same table.

## Routed services

A routed service has one copy, the baseline's, for every worktree that didn't
change it. It exists for the expensive process in a stack, usually a frontend
dev server: a branch that only changed the API shouldn't need its own.

```
browser -> :3400 (router, slot 2's port for web)      marks the request: X-Hydra-Worktree: 2
        -> main's web :3000                           copies the mark onto its calls
        -> :8080 + (max_slots+1)*stride (router)      reads the mark
        -> slot 2's api :8480, or main's :8080 when slot 2 runs none
```

- **The router** is one more process in the baseline's project
  (`hydra-router`). It opens a worktree's port for each routed service that
  worktree didn't change, and one port per service that routed services call.
  In the baseline, a routed service's HTTP upstreams point at those ports.
- **Each worktree publishes what it runs** in
  `<git common dir>/hydra-routes/slot-N.json`, on `hydra up` and on every
  watcher update. The router re-reads those files twice a second and only
  honours a worktree whose process-compose server answers, so `hydra down`
  closes its ports.
- **Port handover.** A worktree's port for a routed service belongs to the
  router until the worktree changes the service. The watcher then publishes
  first, waits for the router to let go, and only then starts the service.
- **Passing the mark on** is the service's job. `routed: node` loads
  `hydra-hook.cjs` with `NODE_OPTIONS=--require`: it keeps the incoming
  header in an `AsyncLocalStorage` and adds it to `fetch` and `node:http`
  calls to the router's ports. `routed: true` means the service does it
  itself (a few lines of middleware in any language).
- **Checked, not assumed.** The router asks the hook (`/__hydra/probe`) to
  call each router port with a made-up mark and compares what arrives. If it
  doesn't match, the worktree's port answers 503 with the reason and the
  command for a copy of your own (`hydra up web`).
- **What can't be routed runs here.** Only upstreams whose value carries the
  target's port as an HTTP URL (or the bare port) go through the router. If a
  routed service has any other upstream pointing at something the worktree
  runs (a Temporal namespace, a database URL), the planner starts the service
  in the worktree, unless that upstream is `per_request: true`. Those values
  travel in an `X-Hydra-Upstreams` header (their names in the service's
  `HYDRA_PER_REQUEST`). The app can read them with
  `globalThis.__hydra.upstream(name)`.
- **Temporal namespaces switch without app changes.** The hook replaces each
  `@temporalio/client` client class with a subclass whose
  `options.namespace` is a getter: while a worktree's request is being
  served, a namespace equal to main's value of a per-request variable reads
  as the worktree's value. The SDK reads `options.namespace` on every call
  (checked on 1.17), so workflows started from main's copy land where the
  worktree's worker polls. The probe builds a client and checks the switch
  on the SDK the app resolves; if it fails, the worktree runs its own copy.
  Calls with no request behind them stay in main's namespace.
- **No silent fallback.** A marked call for a worktree that runs its own copy
  of the target gets a 503 while that copy is down. Unmarked calls, and marks
  of worktrees that don't run the target, go to the baseline's.
- **Next's data cache** (`fetch` with `force-cache` or `revalidate`,
  `unstable_cache`) is keyed without the mark, so the hook keeps a set of
  entries per worktree by prefixing the key. This patches a Next internal
  (`IncrementalCache`); verified on Next 16.4.

Not covered: HTTP/2 and gRPC between services, calls that no request is
behind (timers started at boot, webhooks from outside) which always reach the
baseline's copy, and unchanged services that aren't routed (a baseline worker
still calls the baseline's API).

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
  baseline. Worktree ports for a routed service share `localhost` cookies
  with the baseline, which matters once a worktree has its own database.
- **Webhooks.** A tunnel reaches the baseline's API only, so a provider's
  callback for work a worktree started lands on main. The router could take
  the tunnel's traffic and route by a mark in the callback URL.
- **Shared Redis and queues.** Pub/sub channels and cache keys are shared
  between code versions; a worktree needs its own database index or prefix.
- **PR bases as a parent source.** Stacked PRs already record the parent
  branch on GitHub; reading it would remove most `parent:` overrides.
