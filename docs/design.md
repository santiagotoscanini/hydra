# Design

## Usage

`hydra` stands in for `process-compose`; arguments pass through.

- In the **main checkout** it runs everything, like plain process-compose
  (the server stays on its TCP port), minus `watch` blocks and with each
  `baseline_command` in place of the `command`: main's code only changes on
  `hydra update`. Starting it records the commit and the server's address
  in the shared `.git` directory (`hydra-baseline.json`), where every
  worktree reads them.
- In a **worktree** it runs only the services whose paths that worktree
  changed, on its own ports, wired to the baseline (or to each other). The
  worktree's process-compose server listens on its own unix socket, so
  `hydra attach`, `hydra down` and `hydra process ...` reach that worktree's
  instance and never the baseline's. Before it starts anything, it runs the
  checks of `hydra status` and stops on a ✗ (`--force` starts anyway).
- `hydra up web` in a worktree also runs `web` there, changed or not, and
  switches it on if it is `disabled: true`.
- A file without an `x-hydra` block runs unchanged.
- A worktree without its own `process-compose.yaml` uses the main checkout's.
- process-compose is started without the variables that mark an AI agent's
  shell (`AI_AGENT`, `CLAUDECODE`, `CLAUDE_CODE_*`, `CURSOR_AGENT`, ...):
  dev servers act on them (Next.js prints a notice and exits), and a stack
  has to run the same whoever started it.

## The three sentences

1. The main checkout runs the one full stack, and it only changes on
   `hydra update`.
2. A worktree runs what its branch changed and takes the rest from main.
3. `hydra status` says when either isn't true, with the fix, and `hydra up`
   won't start a worktree on top of something broken.

Everything below serves one of the three. What would break them silently
gets a check instead of a convention: a main that moved by itself, a branch
older than the services it takes, a changed file no service lists, a
process that never came up.

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

## Status

`hydra status` plans the checkout, asks the two process-compose servers what
runs (main's over TCP, the worktree's over its socket), and compares. A
problem is an error (✗: exit 1, and `hydra up` refuses) or a note (!).

| Check | Error when |
|---|---|
| Main's stack | nothing answers at the recorded address |
| What this worktree takes from main | a service it calls, waits for or is routed to has failed or was stopped (with the end of its log) |
| Main's stack against main's files | the checkout moved since the stack was last brought to a commit, and a process this worktree uses has files in between |
| Branch against main | main changed a service the worktree takes from it since the branch left main, or main is older than the branch's starting point (see below) |
| This worktree's stack | a process that should run here failed, was skipped (the step it waited for is shown) or isn't running; `hydra-watch` is down |
| Routed services | the router doesn't answer on the worktree's port, or main's copy doesn't pass the worktree's tag on (`/__hydra/status` on that port) |

Notes: a process that is still starting (`--wait` waits for these), main's
stack file or hydra being newer than the running stack, the main checkout
on another branch or with uncommitted changes, plan warnings, and changed
files that no service's `paths` (nor `ignore`) cover.

"What this worktree takes from main" is: the upstream targets and
`depends_on` entries of its local, enabled services that don't run here, and
every routed service.

`--all`, or `hydra status` in the main checkout, prints main's processes and
one line per worktree without taking a slot for any of them.

### In step with main

Let B be the commit where the branch (or the bottom of its stack) left main,
and M the commit the main checkout has checked out. They differ in the files
`git diff B M` lists. For each service the worktree takes from main, those
files are matched against its `paths`:

- A hit with B older than M: the branch is **behind main**. Merge main.
- A hit with M older than B (a worktree cut from `origin/main` before the
  main checkout pulled): **main is behind**. `hydra update`.

Services running in the worktree are left out, since they run the branch's
code. So is a job in main (migrations) whose files a local service takes:
a worktree with its own database applies its own. Nothing is started or
stopped because of this check. It used to be that a worktree behind main's
migrations got a database of its own; one update of main would then have
started a database, an API and a frontend in every running worktree.

## Updating main

Worktrees take the baseline's running services, so main moves only through
`hydra update`, never by itself: no auto-pull from an editor or agent app,
no cron. Fetching is fine (it moves `origin/main` only).

1. **Pull.** `git pull --ff-only` in the main checkout, which must be on the
   base branch with no uncommitted tracked changes. No upstream: no pull.
2. **Main's stack**, if it is running:
   - The file is rendered again. If it differs from the one the stack was
     started from, `process-compose project update` applies it, and a
     process that was running by hand (`disabled: true`) is started again
     if the reload stopped it. The summary line says which processes the
     reload changes, adds, drops or switches on.
   - `git diff --name-only <recorded commit> <new commit>` gives the files;
     every process whose `paths` match and that has run is restarted, in
     `depends_on` order, each waited for: a job until it completes (a job is
     a process something waits on to complete, or one that had exited 0 and
     has no readiness), a service until it is ready (probe or
     `ready_log_line`; without either, up for three seconds).
   - Not restarted: daemons (`is_daemon`, they hold data; a note says when
     their files changed and nothing else took them), and processes that are
     `disabled: true` unless they are running.
   - A process that had failed is run again even if its files didn't change.
   - The first failure stops the sequence and leaves the record where it
     was, so `hydra status` keeps saying what is stale and the next update
     starts from the same commit. Only a full pass records the new commit.
   - With no record (a stack started by an older hydra), every process with
     `paths` is restarted once.
3. **Worktrees**, parents of a stack first. For each: `git merge-tree
   --write-tree` tries the merge of its parent (main, or the branch it is
   stacked on) in memory. Then: fast-forward when the branch has no commits
   of its own; `git merge --no-edit` when the merge is clean; nothing when
   it conflicts (files and command are printed), when tracked files have
   uncommitted changes, when a merge or rebase is in progress, when no
   branch is checked out, or when the merged tree equals the parent's (the
   branch is already in main, squash-merged for instance). After a merge in
   a worktree whose stack runs, the processes whose `paths` match the merged
   files are restarted there, in the same way as in main.

`--dry-run` prints all of this without doing it, `--main-only` skips step 3,
`--no-pull` step 1. One update runs at a time per repository (a lock in the
shared `.git` directory). Conflicts, generated files and pushing stay with
the person or agent who owns the branch.

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

Commands of its own: `hydra status` (is it all up and in step), `hydra
update` (move main and bring everything along), `hydra plan` (what runs
where, and why), `hydra render` (the generated file), `hydra slots` (which
worktree has which slot).

## The `x-hydra` extension

Hydra reads the project's normal `process-compose.yaml`, with an `x-hydra` key
at the top level and on each process. process-compose ignores keys it doesn't
know (checked on v1.124).

```yaml
x-hydra:
  base_ref: develop       # optional: the branch worktrees are diffed against (default: detected, see below)
  slot_stride: 200        # slot N listens on port + N*200 (slot 0 = baseline)
  max_slots: 20
  reserved_ports: [6379, 7233]  # shared infra; no slot may land here
  global_paths: [go.work] # affect every service that declares paths
  ignore: ["docs/**", ".github/**"]  # belong to no service; other unlisted changes are reported

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
    x-hydra:
      follow: true                # runs here whenever a local service depends on it
      paths: ["pnpm-lock.yaml"]   # and `hydra update` runs it again when a pull changes this
  migrate:
    x-hydra: { shared: true, paths: ["db/migrations/**"] }  # main's job; rerun by `hydra update`
  docs:
    x-hydra:
      paths: ["apps/docs/**"]
      baseline_command: "pnpm build && pnpm start"  # main's copy: built once, no dev server
  supabase:
    x-hydra:
      kind: supabase              # a stack of its own where the schema differs (see Kinds)
      ports: { db: 54322 }        # more ports, shifted per slot, reached as {port.db}
      paths: ["supabase/migrations/**", "supabase/seed/**"]
  redis:
    x-hydra: { shared: true }     # always baseline; warns when its paths change
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
   Naming one also switches it on if it is `disabled: true`.
3. Services with a route override, plus the target of an `=local` override.
4. `follow` services that a local service depends on.
5. Services with an upstream pointing at a service that runs here through a
   value no proxy can route (a database URL, a namespace, a queue name),
   unless it is `per_request`. Main's copy would keep using main's target.
   HTTP calls are left alone: a routed service routes them, and a plain one
   keeps calling the baseline.
6. Minus `shared` services and anything in `--without`.

A changed file that matches no service's `paths` (a `!` exclusion counts as
a match: someone decided), no `global_paths` and no `ignore` entry is
reported by `hydra plan` and `hydra status`. A `shared` service whose paths
changed is a warning only when no service running here takes the same file.

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
| `supabase` | `port: 54321`, `ports.db: port + 1` | In a worktree, a stack of its own (below) |

`hydra prepare temporal` creates the namespace if needed, waits until
Temporal knows it, and adds every custom search attribute the baseline's
namespace has. It is safe to run again and reads `TEMPORAL_ADDRESS` like the
temporal CLI. Calls that time out are tried again: the CLI checks its
connection against the namespace before every command, and a namespace that
was just created often isn't known everywhere yet. A kind can't be `shared`, `follow` or `routed`: its point is a
separate copy per worktree.

### `kind: supabase`

The baseline's Supabase is the user's command, untouched (classic
`supabase start`, its data in the usual Docker volumes). A worktree's copy is
a Supabase CLI stack (`supabase stack`, stack mode): its identity is the
worktree's path plus its branch, so each worktree has its own containers and
data. Rendering adds to the worktree's process:

- `SUPABASE_EXPERIMENTAL_STACK=1`, every port setting on the slot's (the API
  and `ports.db` shifted per slot, the rest at fixed offsets from the API:
  studio +2, mail +3..+5, analytics +6/+7, pooler +8, shadow −1, inspector −2)
  and `SUPABASE_EDGE_RUNTIME_DENO_VERSION=2` (stack mode rejects other
  versions). Ports written in `config.toml` would otherwise be taken as exact
  requests and clash with the baseline's; port 0 is rejected, so hydra picks.
- `TMPDIR` inside the worktree (`supabase/.temp/tmp`): the CLI bind-mounts
  temp files into containers, and Colima's VM only sees the home directory.
- After the command, `hydra prepare supabase`: `supabase migration up
  --include-all` (a resumed stack doesn't apply new migrations on its own),
  then puts back `supabase/.gitignore` when the CLI's appended lines are all
  that changed, and adds the CLI's lock and link files to `info/exclude`. It
  refuses to run without stack mode, which would migrate main's database.
- The shutdown command exports the same environment itself: a `supabase
  stop` without it stops the baseline's Supabase.

The watcher applies migrations that appear while the stack runs, and keeps a
block of the same settings in the worktree's root `.env.local` (never through
a symlink), so CLI commands typed at the worktree's root reach its stack. The
CLI reads `.env.local` only from its working directory, so from a
subdirectory they still reach main's database.

Checked on CLI 2.122.1 with the work monorepo: a fresh stack applies 3,166
migrations and 112 seed files in about 20 s; a resumed one starts in 2 s; an
idle stack is about 110 MB (Postgres only; the rest start on demand). Its
keys and JWT signing key are the CLI's local defaults, the same as the
baseline's, so only URLs differ.

New kinds (SQS queues, Redis key prefixes) go in the same table.

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
  in the worktree, unless that upstream is `per_request: true`. A routed
  service that reaches a service through such a value also calls that
  service's HTTP endpoints directly rather than through the router: a
  worktree running that service runs its own copy of the routed one anyway. Those values
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
- **Redirects stay on the worktree's port.** Main's copy builds absolute
  URLs from its own environment (`NEXT_PUBLIC_SITE_URL` is main's). A
  `Location` that points at main's address for the service is rewritten to
  the address the browser used, or the next requests would be main's,
  untagged. Links inside a page or an email are not rewritten.
- **`/__hydra/status`** on a worktree's port is answered by the router
  itself: whether main's copy passed the check, and why not. `hydra status`
  reads it.
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

- **A worktree's database starts empty of main's data.** It is migrated and
  seeded from the worktree's files. Copying main's data would need a dump
  per start; a template database would tie the stack to the baseline's
  Postgres.
- **Auth redirects.** A worktree's Supabase sends email and OAuth redirects
  to `site_url` from `config.toml`, main's frontend. Setting
  `SUPABASE_AUTH_SITE_URL` to the worktree's frontend port would fix it when
  that frontend runs there.
- **Routed frontends and a worktree database.** A worktree with its own
  Supabase runs its own frontend, because the frontend reads the database
  URL once. Sharing main's would need per-request database clients.
- **Scheduled work.** A worker that sets up its schedules when it starts
  would do so in each worktree's Temporal namespace too, and cron-like jobs
  would run once per worktree against the shared database. (The monorepo
  this was built on creates schedules out of band, so it isn't affected.)
- **Ports are per repository.** Slots are handed out per repository; two
  repositories whose stacks use the same ports can't run at once.
- **Dependency paths by hand.** `paths` lists workspace packages manually.
  They could be derived from the nx project graph or `go list -deps`. Go
  package-level deps would also tell api changes apart from worker changes.
- **Hard-coded ports** in app code or dev scripts need a small patch each
  (or a command override in the process-compose file).
- **Routing to another worktree** (`web.api=slot:3`), and hostnames instead of
  ports (`api.slot1.localhost`), so cookies and CORS behave as on the
  baseline. Worktree ports for a routed service share `localhost` cookies
  with the baseline; a frontend whose cookie names its database (an upstream
  like `sb-hydra-{slot}`) keeps sessions apart.
- **Webhooks.** A tunnel reaches the baseline's API only, so a provider's
  callback for work a worktree started lands on main. The router could take
  the tunnel's traffic and route by a mark in the callback URL.
- **Shared Redis and queues.** Pub/sub channels and cache keys are shared
  between code versions; a worktree needs its own database index or prefix.
- **PR bases as a parent source.** Stacked PRs already record the parent
  branch on GitHub; reading it would remove most `parent:` overrides.
