# Design

The rules, and the decisions behind them. The manual for someone using hydra
is <https://hydra.toscanini.me/docs/> (`site/docs/index.html`).

A word on words: the **baseline** is the main checkout's stack. The config
keys and flags use that name (`baseline_command`, `--route web.api=baseline`)
and so do these notes; everything hydra prints says `main`.

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
  switches it on if it is `disabled: true`. The name goes into the rendered
  file and is not handed to process-compose, which would start `web` alone
  and leave what the branch changed off. In the main checkout the name
  passes through, as process-compose's own `up web`.
- `hydra up` on a stack that is already up starts nothing: a second
  process-compose would fail on the taken socket or port, unseen with `-D`.
  hydra asks its own server first (and checks that what answers there was
  started from this checkout's file: another repository on the same port is
  an error with the way out). Then `-D` says the stack is up and exits 0, a
  terminal gets `process-compose attach`, and `hydra up NAME` adds the names
  to the ones the stack was started with (read back from the watcher's
  command line in the rendered file), renders again and lets
  `process-compose project update` take the difference; in the main
  checkout it starts the named process.
- In the main checkout, a command that talks to the running stack (`down`,
  `attach`, `process ...`, a second `up`) first looks where this shell
  would start process-compose (its flags, `PC_PORT_NUM`, the default). If
  nothing answers there and the recorded address does, with this checkout's
  file, hydra passes that address on: the stack is reached from any shell,
  whatever environment started it.
- `hydra down` on a stack that isn't running says so and exits 0.
  `hydra down --worktrees` stops every worktree's stack and leaves the main
  checkout's: nothing stops an idle stack by itself.
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

**The update goes to process-compose in dependency order.** One `project
update` is applied in no order: process-compose adds the new processes and
replaces the changed ones as it iterates over a map, and a process whose
dependency isn't in the project at that moment logs an error and starts
without waiting; one whose dependency is still the old process (a stand-in
that is "ready", say) is satisfied by that. In the monorepo, a migration
added to a worktree whose API and worker already ran restarted them against
the stand-in's readiness, half a second before the worktree's own database
started: the worker found no tables and hung. In the update demo, a service
waiting for an API that had just moved into the worktree was skipped ("the
process it waits for was terminated") on some runs and started too early on
others. So `applyProject` splits the change (`updateSteps`): the first file
is what runs now plus every changed process that waits for nothing else that
changed (stand-ins, hydra's steps); the next adds what waits for those; the
last is the new file itself, which also removes what is gone. Each call
returns once its processes are registered, so the next step's processes find
their dependencies in the project and wait for them the way the stack file
says. `hydra-watch` only changes with the last file: a new watcher coming up
halfway would start on the same steps. The file on disk is each step in
turn; when one fails it is put back to the last that went through, so the
next tick compares against what really runs. `hydra update` reloads main's
file through the same function.

process-compose's `watch` needs help to work here. It builds its file watcher
once, as the project starts, and only if some process has a `watch` then; a
process added later through `project update` is registered with that watcher
if it exists, and silently not at all if it doesn't. A worktree usually
starts with nothing of its own to run, so its first service arrives through
`project update`, and would never restart on an edit: the second change to
a Go file in the monorepo was not picked up. So when the stack file uses
`watch` anywhere, `hydra-watch` is rendered with a `watch` of its own on an
empty directory (`.process-compose/watch-anchor`) and an include that matches
nothing. It never fires; it makes the watcher exist.

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
| This worktree's stack | a process that should run here failed, was skipped (the step it waited for is shown) or isn't running; `hydra-watch` is down. A process whose output says its port was taken comes with the program listening there (`lsof`): one started from inside this checkout is a leftover of an earlier run, and the fix kills it; anyone else's is only named |
| Routed services | the router doesn't answer on the worktree's port, or main's copy doesn't pass the worktree's tag on (`/__hydra/status` on that port) |

Also an error: a migration that didn't apply to a worktree's own running
database (what the watcher kept of the CLI's output is shown), and, after
`--wait` ran out of time (`--timeout`, 10 minutes by default), whatever is
still starting: a wait that gives up is not "ok". `--wait` reads git and
plans once every 10 seconds and only asks the two process-compose servers
in between (a full look costs about a second of CPU in the monorepo, which
made a ten-minute wait cost six minutes of it); before it reports "settled"
it looks at everything once more, so an edit made a moment ago isn't missed.

Notes: a process that is still starting (`--wait` waits for these), main's
stack file or hydra being newer than the running stack, the main checkout
on another branch or with uncommitted changes, plan warnings, changed
files that no service's `paths` (nor `ignore`) cover, a worktree's own
Supabase that was started for another branch than the one checked out now,
and (in the main checkout) a removed worktree whose stack still answers.

"What this worktree takes from main" is: the upstream targets and
`depends_on` entries of its local, enabled services that don't run here, and
every routed service.

`--all`, or `hydra status` in the main checkout, prints main's processes and
one line per worktree without taking a slot for any of them.

### The page

`hydra ui` serves the same facts for every checkout at once as a local web
page (127.0.0.1:4937; one page per repository, a second repository takes
the next free port, and a second `hydra ui` for the same one opens the
first). It is one HTML file inside the binary and one JSON endpoint,
`/api/status`; it only reads.

- Process states, and the memory each process holds with its children
  (process-compose reports the process alone, which for a wrapped command is
  the wrapper), are read on every request, at most once a second.
- A worktree's plan needs git to walk its working tree for untracked files,
  which is the expensive part with a dozen worktrees. Plans are cached and
  worked out again in the background: every 10 seconds for a worktree whose
  stack is up, every 90 otherwise, or all at once from the page's refresh
  button. The page stops asking while its tab is hidden.
- A worktree that never ran has no slot, so the page shows what it would
  run without ports.
- Links, here and in what `hydra up` prints, name `localhost` where services
  are wired to each other by `127.0.0.1`. A dev server treats a page opened
  by IP address as another origin: Next.js then refuses it the hot-reload
  socket and its dev resources.

It isn't part of the generated file. To have it up whenever main is, add it
to the stack file as an ordinary process (`command: hydra ui --no-open`).

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
   - Waiting for a restarted process goes by what process-compose does: it
     stops the old run, waits the process's backoff, and builds a new state
     for the new run, which reads "Pending" while the run waits for what it
     depends on, or "Disabled" when the process is `disabled: true` (a voice
     agent someone started by hand, say). Both mean "not yet". Only
     "Skipped" means process-compose gave up on it, because something it
     waits for failed. Taking "Disabled" for a failure once made every
     update of a stack with such a process fail, with nothing wrong.
   - Each line is printed when its step ends, not at the end: an update
     takes minutes.
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

Commands of its own: `hydra status` (is it all up and in step), `hydra ui`
(the same as a web page), `hydra update` (move main and bring everything
along), `hydra plan` (what runs where, and why), `hydra render` (the
generated file), `hydra slots` (which worktree has which slot, and freeing
one), `hydra trust` (the CA behind TLS fronts) and `hydra version` (the
commit it was built from, read from the build's VCS stamp).

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
  db:
    x-hydra: { shared: true, port: 5432, tls: 5443 }  # the router answers TLS on 5443 for main's copy (see TLS fronts)
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
  same name, in the `main` namespace. It runs nothing and copies only the
  readiness probe (or ready log line). `process_healthy` therefore waits for
  the baseline's Supabase instead of starting a second one, and stopping the
  worktree never runs the baseline's shutdown commands.
- A `hydra-prepare-<service>` step before each service with `prepare`, with
  `HYDRA_SERVICE`, `HYDRA_SLOT` and `HYDRA_ROOT` (the worktree that holds the
  slot) in its environment.

## Kinds

`kind` names a service Hydra knows more about than its port. It fills in the
keys you'd otherwise write by hand; any key you set yourself wins.

| Kind | Fills in | Also |
|---|---|---|
| `temporal` | `url: hydra-{slot}`, `baseline_url: default`, and a prepare step `hydra prepare temporal --namespace hydra-N --from default` | Routed services switch upstreams pointing at it per request (`per_request` is implied). A namespace left by the slot's previous worktree is replaced |
| `supabase` | `port: 54321`, `ports.db: port + 1` | In a worktree, a stack of its own (below) |

`hydra prepare temporal` creates the namespace if needed, waits until
Temporal knows it, and adds every custom search attribute the baseline's
namespace has. It is safe to run again and reads `TEMPORAL_ADDRESS` like the
temporal CLI. Calls that time out are tried again: the CLI checks its
connection against the namespace before every command, and a namespace that
was just created often isn't known everywhere yet.

The namespace is named after the slot, and slots are handed on. So the
namespace's description records the worktree it was made for
(`hydra worktree <root>`, from `HYDRA_ROOT`). When the step finds a
namespace made for another worktree, it deletes it and creates it again,
empty: otherwise the new worktree's worker would pick up the workflows the
old one left running and run them with another branch's code. Temporal
renames a deleted namespace at once, so the name is free immediately, but
its services cache which namespace a name stands for; the step waits a few
seconds after a replacement for that to pass. A namespace with no such
description (an older hydra made it) is kept and labelled. The same
worktree's namespace survives restarts.

A kind can't be `shared`, `follow` or `routed`: its point is a separate copy
per worktree.

### `kind: supabase`

The baseline's Supabase is the user's command, untouched (classic
`supabase start`, its data in the usual Docker volumes). A worktree's copy is
a Supabase CLI stack (`supabase stack`, stack mode), with its own containers
and data. Rendering adds to the worktree's process:

- `SUPABASE_EXPERIMENTAL_STACK=1`, every port setting on the slot's (the API
  and `ports.db` shifted per slot, the rest at fixed offsets from the API:
  studio +2, mail +3..+5, analytics +6/+7, pooler +8, shadow −1, inspector −2)
  and `SUPABASE_EDGE_RUNTIME_DENO_VERSION=2` (stack mode rejects other
  versions). Ports written in `config.toml` would otherwise be taken as exact
  requests and clash with the baseline's; port 0 is rejected, so hydra picks.
- `TMPDIR` inside the worktree (`supabase/.temp/tmp`): the CLI bind-mounts
  temp files into containers, and Colima's VM only sees the home directory.
- Around the command, `hydra prepare supabase`. `--before`: see the next
  paragraph. `--started $?`, which runs whether or not the start worked:
  it puts back `supabase/.gitignore` when the CLI's appended lines are all
  that changed and adds the CLI's lock and link files to `info/exclude`;
  then, if the start worked, `supabase migration up --include-all --db-url
  <the slot's database>` (a resumed stack doesn't apply new migrations on
  its own). It refuses to run without stack mode, which would reach main's
  database.
- The shutdown command exports the same environment itself (a `supabase
  stop` without it stops the baseline's Supabase), and is followed by
  `hydra prepare supabase --stopped`.

**hydra goes by the directory, not by the CLI's idea of the current
stack.** The CLI derives a stack's id from the project root, the stack's
name and the branch in `HEAD` (`refs/heads/NAME`), with no way to override
the last. A worktree whose branch is renamed (apps that manage worktrees do
this when they name a task) or switched therefore has a running stack that
`supabase start`, `supabase stop` and `supabase migration up` no longer
find, still holding the slot's ports. So:

- migrations are applied through the database's address, not through the
  CLI's lookup;
- `--before` stops the stacks this directory has for other branches
  (`supabase stack list`, matched on `project_root`, stopped by id), so the
  start can have the ports; they keep their data, and checking the first
  branch out again brings its stack back;
- `--stopped` stops whatever stack of this directory still runs;
- `--started` records the branch in `.process-compose/supabase-branch`, and
  `hydra status` notes when it differs from the branch checked out now:
  the services keep working, but `supabase` commands typed in the worktree
  won't find the stack until the process is restarted;
- when the worktree is removed, every stack of that directory is destroyed
  (below).

**Ready means started and migrated.** The stack's readiness probe (the REST
API answering) says the containers are up, not that the schema is there: a
resumed stack answers at once and gets the branch's new migrations a moment
later, from `--started`. A worker that starts in between finds tables
missing. So `--started` writes `.process-compose/supabase-branch` only after
the start worked and `migration up` went through; `--before`, `--stopped`,
a start of the worktree's stack and an update that is about to start the
database all remove it. A step, `hydra-ready-<service>` (`hydra prepare
supabase --ready`), ends when the file is there, and every local service
that waits for the database or is given its address depends on that step
with `process_completed_successfully`, in place of its `depends_on` on the
database. They stay pending meanwhile, so no readiness probe of theirs runs
out during a first start. A wait inside each service's own command was the
alternative; process-compose stops a process whose readiness probe fails too
often, and a first start of a large schema takes longer than a default
probe allows. The step never gives up: when the start fails, `hydra status`
shows the database as failed, and after `hydra process restart supabase`
what was waiting starts by itself. It exits when the process-compose that
started it is gone.

The watcher applies migrations that appear while the stack runs (`hydra
prepare supabase --migrate`, which leaves the branch record alone), once the
stack is ready: before that the start applies them all, and the first look
after it applies them once more, quietly, in case a file landed while the
start was ending. When that fails, the end of what the CLI said is kept in
`.process-compose/supabase-migrations-failed`, which `hydra status` reports
as an error until a later run applies them. The watcher also keeps a
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

## TLS fronts

A baseline process may need `https` where the service speaks plain HTTP: a
production build whose Content-Security-Policy allows `https:` and `wss:`
only, or an SDK that turns TLS on whenever an API key is set. `tls` on a
service adds a front for the baseline's copy:

```yaml
supabase:
  x-hydra: { port: 54321, tls: 54421 }
temporal:
  x-hydra: { shared: true, port: 7233, tls: { port: 7243, alpn: h2 } }
```

- **The router terminates TLS and nothing else.** After the handshake it
  copies bytes both ways to the service's port. It never parses them, so the
  front carries whatever the service speaks in the clear, provided ALPN names
  it: `http/1.1` (the default, websockets included) or `h2` for a service that
  takes HTTP/2 without TLS, as gRPC servers do. Offering one protocol only is
  what keeps a browser from speaking HTTP/2 to an HTTP/1.1 service.
- **Main's copy only.** A front is opened for the baseline's port. The
  address is given with `baseline_value` on an upstream, which may use
  `{tls}`; `value` and `url` may not, because a worktree's own copy has no
  front. So main's process gets `https://127.0.0.1:54421` and a worktree's
  gets the plain address, as before. Such an upstream isn't sent through the
  router's per-worktree dispatch. A baseline process with a `{tls}` value
  waits for `hydra-router`, which exists as soon as a service has `tls`.
- **The certificate.** hydra keeps one CA per user
  (`<user config dir>/hydra`: `ca.pem`, `ca-key.pem`, `cert.pem`, `key.pem`;
  `HYDRA_TLS_DIR` moves it), so trusting it once covers every repository. The
  CA is ECDSA P-256, valid ten years, path length 0, and name constrained
  (critical) to `localhost`, names under it, `127.0.0.0/8` and `::1`: a
  certificate for any other name signed with its key is refused. The
  certificate covers `localhost`, `*.localhost`, `127.0.0.1` and `::1` for
  800 days (Apple's limit is 825) and is made again when it is missing, a
  month from expiry, or signed by another CA. The router reads it at every
  handshake, so a new one needs no restart. `x-hydra.tls: { cert, key, ca }`
  names a stack's own files (paths relative to the stack file), which hydra
  only reads.
- **Trust.** `hydra trust` adds the CA to the user's keychain, trusted for
  TLS only (macOS; elsewhere it prints the file). `--remove` undoes it,
  `--print` prints the path. Whether the machine trusts the certificate is
  asked of the system verifier, and `hydra status` notes when it doesn't.
  A baseline process that is given a front's address gets
  `NODE_EXTRA_CA_CERTS=<ca>`, unless it sets its own; no other process is
  touched, so adding a front to a running stack restarts nothing.
  Firefox keeps its own list of authorities.
- **`hydra status`** fails when a front isn't accepting connections.

Two things learned on the monorepo this was built for:

- grpc-js on recent Node refuses TLS to an IP address (it can't put one in the
  TLS server name), so a gRPC client's address has to be `localhost:<port>`.
- Don't put a front before a web app that sends `Strict-Transport-Security`
  and open it as `https://localhost`: HSTS is kept per host name, not per
  port, so the browser would insist on https for every port on localhost.
  Fronts are for the services a page talks to; the page itself can stay on
  http, where `'self'` covers it. (An IP address is never pinned.)

## Slots

`hydra-slots.json` in the shared `.git` directory maps worktree paths to
slots, guarded by flock. A worktree keeps its slot across runs. Ports for
every slot up to `max_slots` are checked against each other and
`reserved_ports` when the config loads. They are computed, not probed:
another program may sit on one, which `hydra status` then names.

A slot stays taken until it is released. An entry whose directory is gone
is not dropped on sight, as it once was: whatever that worktree ran may
still hold the slot's ports.

## Removed worktrees

Nothing tells hydra that a worktree was removed: git has no hook for it,
and tools that manage worktrees delete the directory. A worktree removed
with its stack up used to leave process-compose and its services running
with no directory under them, on ports the freed slot handed to the next
worktree, and its Supabase stack's containers and data for good. Now:

- **The stack stops itself.** `hydra-watch` checks on every tick that its
  worktree's directory exists. When it doesn't, it destroys the Supabase
  stacks made for that directory and runs `process-compose down` on its own
  project, which is the last thing it does. (process-compose's own shutdown
  commands fail there, having no directory to run in.)
- **The next start finishes the job.** `hydra up` (any checkout, before it
  takes a slot), `hydra update` and `hydra down --worktrees` go through the
  slots whose directory is gone: stop the stack if its socket still answers
  (the socket's path is derived from the worktree's path, so it can be
  found without the worktree), destroy its Supabase stacks, remove its
  routes file and socket, and only then release the slot. A step that
  fails leaves the slot taken, with a warning, and the next start tries
  again.
- **`hydra slots free PATH`** does the same for one worktree, removed or
  not. For one that is still there, the Supabase stack is stopped, not
  destroyed.
- **Temporal** needs no cleanup at removal: the namespace is replaced when
  the slot's next worktree prepares it (see Kinds), which also works when
  Temporal was down at the time of the removal.
- **A killed process-compose** is the other way to leave things behind: the
  services live on and keep their ports. The watcher and the router compare
  their parent process on every tick and exit when it changes, so two
  watchers never update one project and two routers never fight over a
  port. The services are the stack's own programs; `hydra status` names
  each one in the way of a restart.

A `prepare` command of the stack file has no counterpart that runs at
removal. What it creates is named after the slot, so it is found again by
the slot's next worktree; a step that must start clean has `HYDRA_ROOT` to
tell whose it was.

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
- **Idle stacks.** A worktree's stack runs until someone stops it or the
  worktree is removed. With many worktrees and agents that start stacks and
  move on, memory fills up. `hydra down --worktrees` and the status page's
  memory column are what there is; a watcher that stops a stack nobody has
  touched for some hours would need a definition of "touched" that doesn't
  stop one a person is clicking through.
- **Ports are computed.** A slot's port can already be taken by another
  program (anything above 49152 is in the range the system hands out).
  hydra reports it once a service fails to bind; it doesn't probe first.
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
