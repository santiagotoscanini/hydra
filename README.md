# Hydra

Run many branches of a local stack in parallel without running many stacks.

One **baseline** stack (the main checkout) stays up under
[process-compose](https://github.com/F1bonacc1/process-compose). Each worktree
starts only the services it changed (frontend, backend, database, workers) and
points everything else at the baseline. A frontend-only branch reuses the
baseline API and database, and a schema change gets its own database.

Site and docs: <https://hydra.toscanini.me> (source in `site/`, deployed by
`.github/workflows/pages.yml`).

Status: exploration; the wrapper works against a real stack. Planning lives in the TickTick list
"Hydra".

## Install

```sh
brew tap santiagotoscanini/tap
brew install --HEAD santiagotoscanini/tap/hydra    # builds main
brew upgrade --fetch-HEAD santiagotoscanini/tap/hydra
```

Working on hydra itself: `make link` puts this checkout's build on PATH as
`hydra` (and unlinks Homebrew's), `make build` refreshes it, `make unlink`
goes back.

## Usage

Hydra wraps process-compose. Add `x-hydra` fields to the existing
`process-compose.yaml` (which services own which paths, which env vars point
at which service), then run `hydra` wherever you'd run `process-compose`:

```sh
hydra up -D        # main checkout: everything; worktree: only what it changed
hydra status       # is everything this checkout uses up, and in step with main?
hydra update       # pull main, restart what the pull touched, merge main into the worktrees
hydra plan         # what runs here, on which ports, wired to what, and why
hydra up web       # also run web in this worktree
hydra down         # stops this checkout's instance only
```

The whole model is three sentences:

1. The main checkout runs the one full stack, and it only changes when you
   run `hydra update`.
2. A worktree runs what its branch changed and takes the rest from main.
3. `hydra status` says when either isn't true, with the command that fixes
   it, and `hydra up` won't start a worktree on top of something broken.

## How it works

### What counts as changed

A worktree changed a service when a file that differs from where its branch
left `main` matches the service's `paths`. Committed, staged, unstaged and new
files all count; git-ignored files don't:

```sh
git diff --name-only $(git merge-base HEAD main origin/main)   # committed, staged, unstaged
git ls-files --others --exclude-standard                       # new files
```

`origin/main` counts too because the local `main` is often behind it: a
branch cut from a freshly fetched `origin/main` forked there, and the commits
merged in between aren't its work.

`main` here is the repository's default branch, read from git: the one
`origin` calls its default (`git symbolic-ref refs/remotes/origin/HEAD`),
else a local `main` or `master`, else the branch the main checkout is on.
`base_ref` in the top-level `x-hydra` block names another.

Services left to the baseline run whatever the main checkout has checked
out. When that isn't the code the branch was built on, hydra says so: see
[In step with main](#in-step-with-main).

A changed file that no service lists is easy to miss: nothing runs here
because of it, and you keep getting main's copy of the code you changed.
`hydra plan` and `hydra status` name such files. List the ones that belong
to no service (docs, CI, other apps) under `ignore` in the top-level
`x-hydra` block, and what remains is a path someone forgot.

### Starting

`hydra` in a worktree:

1. Lists the changed files and picks the services to run here.
2. Takes a **slot** for the worktree (kept across runs). Slot N listens on
   each service's port + N × `slot_stride`.
3. Checks the main checkout: its stack is running, the services this
   worktree takes from it are up, and they are the version the branch was
   built on. If not, it prints what is wrong and the fix, and doesn't start
   (`--force` starts anyway).
4. Writes `.process-compose/hydra.yaml`: those services only, on the slot's
   ports, with each upstream env var pointing at the local copy if the
   service runs here and at the baseline otherwise.
5. Starts process-compose on that file, on the worktree's own socket, so
   `hydra down` or `hydra attach` there never touch the baseline.

In the main checkout `hydra` runs the whole file, with two differences from
plain process-compose. `watch` blocks are dropped: main's code only changes
on `hydra update`, which restarts what changed, in order. And a process
with `baseline_command` runs that instead of its `command`, for a build that
fits a checkout nobody edits.

Whoever types `hydra up`, the processes run the same: hydra starts
process-compose without the variables that mark an AI agent's shell
(`AI_AGENT`, `CLAUDECODE`, `CURSOR_AGENT` and the like), which some dev
servers act on. Next.js prints a notice for agents and exits.

A process with `disabled: true` stays off until someone starts it. In a
worktree, `hydra up mobile` is someone starting it.

### Checking

`hydra status` answers "can I trust what runs here?". It plans the checkout,
then asks both process-compose servers what is really running:

```
main      ~/code/shop  on main at 9a782732a  stack up
worktree  ~/code/shop-cart  cart, slot 2  stack up

SERVICE   RUNS          PORT   STATE
api       here          8480   ready
web       main, routed  3400   ready
db        main          5432   ready

✗ this branch is behind main: main changed these since the branch left it, and this worktree uses main's copy:
    web (it serves this worktree's api): apps/web/cart.tsx and 11 more
  fix: hydra update   (or, for this worktree alone: git merge main)
```

It exits 1 when something is marked ✗, so a script or an agent can gate on
it. `hydra status --wait` first waits for whatever is still starting.
`--all` (and plain `hydra status` in the main checkout) lists every
worktree: what it runs, whether its stack is up, and where it stands
against main.

What it checks:

- main's stack is running, and each service this worktree takes from it is
  up (a failed one comes with the end of its log);
- main's stack runs what main has checked out, and its current
  process-compose.yaml;
- the branch and main agree on every service the worktree takes from main
  (next section);
- everything that should run here does, including the steps hydra adds;
- main's copy of a routed service can serve this worktree;
- no changed file is left out of every service's `paths`.

The same checks run before `hydra up` starts a worktree, and the watcher
writes to the worktree's log when main's stack stops answering or the
branch falls out of step.

### Watching

The generated file has one extra process, `hydra-watch`, so the watcher starts
with the services and stops with them. Every 2 seconds it lists the changed
files again. When the set of services to run differs, it rewrites the file and
calls `process-compose project update`, which touches only the processes that
differ. That gives two transitions:

| Transition | What happens |
|---|---|
| unchanged → changed (first matching file) | The service starts here. Local services that point at it restart with its URL. |
| changed → unchanged (last change reverted) | The service stops. Local services that pointed at it restart on the baseline's URL. |

Further edits to a service that already runs here do nothing in Hydra: the
service's own reload (`next dev`, `air`, process-compose `watch`) picks them
up.

It polls git instead of watching the file system because the question is
"does this differ from `main`", which only git answers: a revert has to stop
the service, and ignored files (`node_modules`, build output) must not start
one. A tick costs about 0.35s on a large monorepo and never takes git's index
lock, so it doesn't collide with an agent's own git commands.

### In step with main

A worktree's `web` from last week calling main's `api` from today can fail
in ways that have nothing to do with the branch. So hydra compares the
commit where the branch left main with the commit main has checked out, and
looks at the services the worktree takes from main:

- the ones a service running here calls (its upstreams) or waits for;
- a routed service whose calls land in this worktree.

If main changed one of those services' files in between, the worktree is
**behind main**, and merging main into the branch fixes it. If the branch
started from a newer commit than main is on (a worktree cut from
`origin/main` before main pulled it), **main is behind**, and `hydra update`
fixes it. Either way `hydra up` refuses to start and `hydra status` says
which services and files.

Services the worktree runs itself don't count, however old the branch:
they run the branch's code. Neither do two services that both run in main.

### Updating main

Worktrees take what the main checkout is running, so main moves only when
you say so. Turn off anything that pulls it by itself (an editor's or agent
app's auto-pull, a cron job); fetches are fine, they only move
`origin/main`. Then, whenever you want the latest:

```sh
hydra update              # --dry-run says what it would do
```

1. **Pulls main**, fast-forward only. It stops if the main checkout isn't
   on its branch or has uncommitted changes.
2. **Brings main's stack to the new commit.** It reloads the stack file if
   that (or hydra) changed, then takes every process whose `paths` match a
   file the new commits changed and restarts it, in `depends_on` order,
   waiting for each: a job (`pnpm install`, migrations) to finish, a service
   to be ready. It stops at the first failure, so no service starts on
   migrations that didn't apply, and the next `hydra update` picks up from
   there.
3. **Merges main into every worktree that is behind it**, after trying the
   merge in memory. It fast-forwards a branch with no commits of its own,
   merges where the merge is clean, and leaves the rest alone, saying why:
   a conflict (with the files and the command), uncommitted work, a branch
   whose changes are all in main already. In a worktree whose stack is
   running it then restarts what the merge touched there. It never resolves
   a conflict, rewrites a commit or pushes.

```
main  ~/code/shop  moved f67f0e32d → 9a782732a (30 commits)

main's stack
  migrate  ran again  db/migrations/0042_orders.sql and 2 more  done in 6s
  api      restarted  services/api/handler.go and 207 more      ready in 9s
  web      restarted  apps/web/app/page.tsx and 242 more        ready in 31s

worktrees
  cart      cart      main merged in (30 commits); restarted there: api
  checkout  checkout  CONFLICTS with main in 2 files (37 commits behind)
  search    search    left alone, 30 commits behind main: 1 file with uncommitted changes
```

Jobs take part by listing `paths` like services do: give the migrations job
the migrations folder and the install job the lockfile. What an update never
does: restart a daemon (`is_daemon: true`, a database), since it holds data,
or start a process that is `disabled: true`, unless someone has it running.

hydra remembers the commit main's stack was last brought to. When main is
pulled some other way, `hydra status` says which processes still run the old
code, and `hydra update` restarts exactly those.

`--main-only` leaves the worktrees alone, `--no-pull` skips the pull.
`./examples/demo/update.sh` runs all of this on a throwaway repository.

### Stacked branches

```
main ← b1 (api) ← b2 (worker) ← b3 (web)      each in its own worktree
```

Running `hydra` in b3's worktree:

| Layer | Diffed against | Its diff holds | Runs |
|---|---|---|---|
| b1 | where b1 left `main` | b1's work | `api`, from b1's worktree |
| b2 | where b2 left b1 | b2's work, not b1's | `worker`, from b2's worktree |
| b3 | where b3 left b2 | b3's work only | `web`, from b3's worktree |

- Each layer is compared with its **parent**, not with `main`.
- A service runs from the **topmost** layer that changed it, in that layer's
  worktree, so an agent editing b2's worktree reaches the worker through the
  worker's own reload.
- Everything runs in b3's process-compose project: one slot, one set of logs.
  b1 and b2 don't need to run `hydra` themselves.
- `hydra-watch` polls every worktree in the stack, so an edit in b1's
  worktree starts or stops services in b3's project.

A worktree's parent is, in order: `parent:` in its `.hydra.local.yaml`; the
branch it was created from (read from its reflog), if that branch is checked
out in a worktree; else the deepest checked-out branch whose tip it contains.
Branches that only share history (siblings) are not parents. `hydra plan`
prints the stack it found and why each service runs where it does.

### One copy for every worktree

A worktree that changed only the API has nothing in front of it: the
frontend still runs in main and calls main's API. Starting a second frontend
in every such worktree works, but a dev server is the expensive part of a
stack. Mark it `routed` and main's copy serves every worktree instead:

```yaml
web:
  x-hydra:
    port: 3000
    routed: node          # or `true` when the service forwards the header itself
    upstreams:
      API_URL: api
```

- Each worktree gets its own port for `web` (slot 2: `:3400`), whether or
  not it changed it. If it did, its own copy listens there. If it didn't,
  Hydra's router does: it marks the request with the worktree (an
  `X-Hydra-Worktree` header) and passes it to main's copy.
- Main's `web` calls the API through the router, which sends each call to
  the marked worktree's API when that worktree runs one, and to main's
  otherwise. Every response says who answered in `X-Hydra-Served-By`.
- In between, `web` has to copy the mark from the request it is serving onto
  the calls it makes. With `routed: node`, Hydra loads a small preload into
  the Node process that does this (and keeps Next's data cache apart per
  worktree), so the app needs no change. The router checks that the mark
  really comes out the other side before it lets a worktree use the copy.

Two rules keep this from quietly testing the wrong code:

- A proxy can only route requests. If `web` also reads, say, a queue name
  from an env var and the worktree runs its own worker, main's copy would
  keep using main's queue. Hydra then runs `web` in the worktree after all.
  The same goes for any service, routed or not, that reaches something the
  worktree runs through such a value: a worktree with its own database runs
  its own API too.
  Temporal is handled for you: mark the worker `kind: temporal` and the hook
  switches the namespace inside the Temporal SDK, so the app isn't changed.
  Any other value can be marked `per_request: true`, and the app reads it per
  request from `globalThis.__hydra.upstream('QUEUE')`.
- When a worktree runs its own API and that API isn't up, calls for that
  worktree get a 503. They never fall back to main's.

`./examples/demo/routed.sh` shows it with a Node frontend that knows nothing
about Hydra.

### Kinds

Some services need more than a port to get a copy per worktree. `kind`
tells Hydra what the service is, and it does the rest:

```yaml
worker:
  x-hydra:
    kind: temporal        # each worktree's worker polls namespace hydra-<slot>
    paths: ["services/core/**"]
```

That creates the namespace before the worker starts (with the baseline
namespace's search attributes), hands it to every upstream pointing at the
worker, and switches it per request on routed services.

```yaml
supabase:
  x-hydra:
    kind: supabase        # a worktree whose schema differs gets its own Supabase
    ports: { db: 54322 }  # reached as {port.db}; the API is on port (54321)
    paths: ["supabase/migrations/**", "supabase/seed/**", "supabase/config.toml"]
```

The main checkout runs its Supabase as written. A worktree that changed its
schema files (a migration, a seed, the config) gets a Supabase CLI stack of
its own (stack mode) on its slot's ports, migrated and seeded from its
files, and everything that reads that database runs in the worktree too.
Every other worktree shares main's, and one that is behind main's migrations
is told to update, like for any other service. An idle stack takes about
110 MB.

See [docs/design.md](docs/design.md) for the `x-hydra` fields and the rules
in full.

`examples/demo` is a mock stack; `./examples/demo/run.sh` starts a baseline
and one worktree and shows which instance each request went through:

```
web-a baseline : web-a(slot 0, :20000) -> api(slot 0, :20010) -> db(slot 0, :20030)
web-b baseline : web-b(slot 0, :20001) -> api(slot 0, :20010) -> db(slot 0, :20030)
web-b worktree : web-b(slot 1, :20101) -> api(slot 1, :20110) -> db(slot 0, :20030)
```

`./examples/demo/stack.sh` builds a throwaway stack of worktrees and shows
the watcher starting and stopping services as files change in them.
`./examples/demo/update.sh` shows `hydra status` and `hydra update` keeping
worktrees in step with main, and checks every step.

## Open questions

- Database: a worktree's own Supabase starts from the seed files, not from
  the baseline's data. Copying main's data in would need a dump per start.
- Ports are per repository: two repositories whose stacks use the same
  ports can't run at once.
- Deriving `paths` from the nx / Go dependency graph instead of by hand.
  Until then, the report of changed files nobody lists is the safety net.
