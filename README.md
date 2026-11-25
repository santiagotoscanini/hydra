# Hydra

Run many branches of a local stack in parallel without running many stacks.

The main checkout runs the one full stack, under
[process-compose](https://github.com/F1bonacc1/process-compose). Each git
worktree starts only the services its branch changed (frontend, backend,
database, workers) and takes everything else from main. A frontend-only
branch reuses main's API and database, and a schema change gets its own
database.

**The manual is at <https://hydra.toscanini.me/docs/>**: setting up a
repository, the commands for every day, what every message means, and every
option. Its source is `site/docs/index.html`. This README is the short
version; `docs/design.md` holds the rules and the decisions behind them.

Status: early, built against one real monorepo. Planning lives in the
TickTick list "Hydra".

## Install

```sh
brew tap santiagotoscanini/tap
brew install --HEAD santiagotoscanini/tap/hydra    # builds main
brew upgrade --fetch-HEAD santiagotoscanini/tap/hydra
hydra version                                      # which build, which process-compose
```

The dashboard (bare `hydra` in a terminal) needs Node.js 20 or later; every
command works without it.

Working on hydra itself: `make link` puts this checkout's build on PATH as
`hydra` (and unlinks Homebrew's), `make build` refreshes it, `make unlink`
goes back.

## The model

1. The main checkout runs the one full stack, and it only changes when you
   run `hydra update`.
2. A worktree runs what its branch changed and takes the rest from main.
3. `hydra status` says when either isn't true, with the command that fixes
   it, and `hydra up` won't start a worktree on top of something broken.

hydra's output says `main` for the main checkout. The config keys and flags
call its stack the baseline (`baseline_command`, `--route web.api=baseline`).
They mean the same thing.

## Day to day

Hydra wraps process-compose. Add `x-hydra` fields to the existing
`process-compose.yaml` (which services own which paths, which env vars point
at which service), then run `hydra` wherever you'd run `process-compose` (a
mistyped command is refused, where process-compose would start the stack).
Bare `hydra` in a terminal opens [the dashboard](#the-dashboard):

| You want to | Run | Where |
|---|---|---|
| Start the full stack | `hydra up -D` | the main checkout |
| Start a branch's stack | `hydra up -D` | its worktree |
| Know when it is ready, or what broke | `hydra status --wait` | the checkout you started |
| See every worktree at once, and act on it | `hydra` (the dashboard), `hydra status --all` | anywhere |
| Look at a process | `hydra attach`, `hydra process logs web` | the checkout |
| Also run a service the branch didn't change | `hydra up worker` | the worktree |
| Bring main and the worktrees to the latest commit | `hydra update` (`--dry-run` first) | anywhere |
| Merge main into one worktree, and move nothing else | `hydra update --here` | the worktree |
| See what would run, and why | `hydra plan` | the checkout |
| Stop one stack | `hydra down` | its checkout |
| Stop every worktree's stack | `hydra down --worktrees` | anywhere |
| See what happened, in every checkout | `hydra log` (`--errors`, `-f`, `--since 24h`) | anywhere |
| Get help with hydra | `hydra report`: one Markdown file to send | anywhere |

Every one of them is safe to run twice: starting a stack that is up, or
stopping one that is down, says so and exits 0. On a stack that is already
up, `hydra up NAME` adds NAME to it.

Everything hydra does and sees goes into one journal per repository
(`hydra-journal/` in the shared `.git`, a JSON-lines file per day, kept 30
days): every command and its outcome, each ✗ and ! a check showed, every
change in a process's state with its last output when it failed, and once a
minute each stack's memory and CPU. `hydra log` reads it; `hydra report`
writes it, with the state of every stack and usage over time, to one file.

Set up once per repository: the `x-hydra` keys; `.process-compose/` and
`.hydra.local.yaml` in `.gitignore`; a port of its own for main's
process-compose (`export PC_PORT_NUM=8099`, since its default 8080 is often
an API's; hydra records where it started, so later commands find it from
any shell); and nothing that moves main by itself (see
[Updating main](#updating-main)).

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

Services left to main run whatever the main checkout has checked out. When
that isn't the code the branch was built on, hydra says so: see
[In step with main](#in-step-with-main).

A changed file that no service lists is easy to miss: nothing runs here
because of it, and you keep getting main's copy of the code you changed.
`hydra plan` and `hydra status` name such files. List the ones that belong
to no service (docs, CI, other apps) under `ignore` in the top-level
`x-hydra` block, and what remains is a path someone forgot.

### Starting

`hydra up` in a worktree:

1. Cleans up after worktrees that were removed since the last start (see
   [Stopping and cleaning up](#stopping-and-cleaning-up)).
2. Lists the changed files and picks the services to run here.
3. Takes a **slot** for the worktree, kept until the worktree is removed.
   Slot N listens on each service's port + N × `slot_stride`. When all
   `max_slots` are taken, it takes the slot of a worktree whose stack isn't
   running (that one gets a slot again when it starts). A slot where a port
   would be one browsers refuse (6000, 10080, ...) is skipped, and so is
   one where another program already answers on a port (macOS's AirPlay
   Receiver holds 5000 and 7000).
4. Checks the main checkout: its stack is running, the services this
   worktree takes from it are up, and they are the version the branch was
   built on. If not, it prints what is wrong and the fix, and doesn't start
   (`--force` starts anyway).
5. Writes `.process-compose/hydra.yaml`: those services only, on the slot's
   ports, with each upstream env var pointing at the copy here if the
   service runs here and at main's otherwise.
6. Starts process-compose on that file, on the worktree's own socket, so
   `hydra down` or `hydra attach` there never touch main's stack.

If the stack is already up, nothing new starts: `hydra up -D` says so, plain
`hydra up` opens the running stack's terminal UI, and `hydra up NAME` adds
NAME to it.

In the main checkout `hydra up` runs the whole file, with two differences from
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
worktree  ~/code/shop-notes  notes, slot 2  stack up

SERVICE  RUNS          PORT  STATE
api      here          8480  ready
db       main          -     ready
web      main, routed  3400  ready
(2 more of main's services aren't used by anything here; `hydra plan` lists them)

✗ this branch is behind main: main changed these since the branch left it, and this worktree uses main's copy:
    web (it serves this worktree's api): apps/web/app/cart.tsx, apps/web/lib/totals.ts and 10 more
  fix: hydra update --here   (or, for every worktree: hydra update)
```

It exits 1 when something is marked ✗, so a script or an agent can gate on
it. A line marked ! is a note. `hydra status --wait` first waits for
whatever is still starting, and counts what still is after `--timeout`
(10m) as an error. `--all` (and plain `hydra status` in the main
checkout) lists every worktree: what it runs, whether its stack is up, and
where it stands against main.

What it checks:

- main's stack is running, and each service this worktree takes from it is
  up (a failed one comes with the end of its log);
- main's stack runs what main has checked out, and its current
  process-compose.yaml;
- the branch and main agree on every service the worktree takes from main
  (next section);
- everything that should run here does, including the steps hydra adds. A
  service that died because its port was taken comes with the program that
  holds the port; when that is a leftover of this stack, the restart it
  suggests stops it first;
- main's copy of a routed service can serve this worktree;
- no changed file is left out of every service's `paths`;
- no port of what this checkout runs is held by a program outside any
  stack (a dev server left by a removed worktree, say): it is named with
  its pid and directory, and the fix is its `kill`.

The same checks run before `hydra up` starts a worktree, and the watcher
writes to the worktree's log when main's stack stops answering or the
branch falls out of step. The manual lists
[every message](https://hydra.toscanini.me/docs/#messages) with its fix.

`hydra status --json` prints all of it, for main and every worktree, as
one JSON object: what each runs and takes from main, its processes, its
problems with their fixes (and the fix as a command when it is a hydra one),
the programs outside any stack that hold its ports (`strays`),
how far it is from main and what `hydra update` would do to it.
`--watch` prints one every second.

### The dashboard

Bare `hydra` in a terminal opens a dashboard of main and every worktree,
built on `hydra status --json --watch`: each checkout's slot, whether its
stack is up, what it runs here, how it stands against main (behind, a
conflict, stacked on another branch), and for the chosen one its services
and the problems with their fixes.

```
hydra  main up · 2/4 worktrees up · read now  /Users/me/code/shop
   CHECKOUT           SLOT STACK     RUNS HERE            VS MAIN
   main               0    up        4/4 ok
 * shop-cart          1    up        web                  2↑
   shop-checkout      -    down      api,web              conflicts(1) 1↓ 1↑
▸  shop-notes         2    failing   api                  1↓ 1↑
   shop-search        -    down      web                  1↓ 2↑ on cart
```

Each key runs the hydra command an agent would type, in the chosen
checkout: `u` `hydra up -D`, `d` `hydra down`, `r` `hydra process restart`,
`l` `hydra process logs -f`, `f` a problem's fix, `m` `hydra update --here`,
`U` `hydra update` (both after their dry run), `R` reads everything again,
`?` lists the keys, `q` quits and leaves every stack running. `⏎` opens the
stack in `process-compose attach`; F10 or ctrl-c, then Yes, comes back with
the stack still running. Whatever stops a stack or moves a branch asks
first. It needs Node.js 20+; without a terminal (an agent, a script) or
with any argument, `hydra` is process-compose as before, and process-compose's
own TUI on a stack is `hydra up` or `hydra attach`.

### Watching

The generated file has one extra process, `hydra-watch`, so the watcher starts
with the services and stops with them. Every 2 seconds it looks at `git
status`, the branches and hydra's files; when any of them moved (and at least
every 30 seconds) it lists the changed files again. When the set of services
to run differs, it rewrites the file and
calls `process-compose project update`, which touches only the processes that
differ. It hands the file over in dependency order, since process-compose
starts what one update adds in no order of its own: a service starts after
the step or the service it waits for, as it does on `hydra up`. That gives
two transitions:

| Transition | What happens |
|---|---|
| unchanged → changed (first matching file) | The service starts here. Services here that point at it restart with its URL. |
| changed → unchanged (last change reverted) | The service stops. Services here that pointed at it restart on main's URL. |

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
app's auto-pull, a cron job), and have new worktrees start from the local
`main` rather than `origin/main`. Fetches are fine, they only move
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
main  ~/code/shop  moved 9a782732a → 66b4f7b80 (4 commits)

main's stack
  api  restarted  services/api/handler.go  ready in 9s
  web  restarted  apps/web/app/page.tsx    ready in 31s

worktrees
  shop-cart      cart      fast-forwarded to main (4 commits; it has none of its own); restarted there: web
  shop-checkout  checkout  CONFLICTS with main in 1 file (4 commits behind)
  shop-notes     notes     main merged in (4 commits); restarted there: api
  shop-search    search    left alone, 4 commits behind main: 1 file with uncommitted changes
```

Jobs take part by listing `paths` like services do: give the migrations job
the migrations folder and the install job the lockfile. What an update never
does: restart a daemon (`is_daemon: true`, a database), since it holds data,
or start a process that is `disabled: true`, unless someone has it running.

hydra remembers the commit main's stack was last brought to. When main is
pulled some other way, `hydra status` says which processes still run the old
code, and `hydra update` restarts exactly those.

`--main-only` leaves the worktrees alone, `--no-pull` skips the pull.
`hydra update --here`, in a worktree, does only step 3, for that worktree:
it merges main (or the branch it is stacked on) and restarts what the merge
touched there. Main isn't pulled.
`./examples/demo/update.sh` runs all of this on a throwaway repository.

### Stopping and cleaning up

`hydra down` stops this checkout's stack. `hydra down --worktrees` stops
every worktree's stack and leaves main's running: a stack costs memory for
as long as it is up, and nothing stops an idle one for you.

A stop never hangs in silence: every 10 seconds it says what it waits for
(`hydra: waiting for redis (40s)`), and a process still stopping after 60
seconds (or its own `shutdown.timeout_seconds` and 10 more) is killed with
its process group, said on the screen and written to the journal.

A worktree can be removed without stopping anything first, with
`git worktree remove` or by the tool that manages your worktrees. Its
watcher notices within a second, and a few seconds later:

- Its stack has stopped.
- A Supabase stack of its own is destroyed.
- Its folder is gone. With a stack up, git often can't delete all of it:
  the stack writes there while git deletes, and git gives up with
  `error: failed to delete '…': Directory not empty`, forgetting the
  worktree anyway. hydra deletes what git left.
- Its slot is free, once nothing of the worktree answers any more. Ports
  are never handed on while something holds them.

When no stack ran there, only the slot is left, and the next `hydra up` in
any checkout (or `hydra update`) frees it. The same start stops a stack
whose watcher wasn't running; from the folder it then only deletes hydra's
own files, and names the folder if anything else is left in it.

If a stack's process-compose is killed, nothing it had started ends with
it. hydra's own process in the stack (the watcher in a worktree, the router
in main) notices, stops every one of those processes, and exits: a second
later `hydra status` says the stack isn't running, and `hydra up -D` brings
it back. What stays up is what process-compose never held as a process, such
as a worktree's own Supabase stack.

When the watcher was killed too (`pkill -f process-compose` matches both),
the processes live on. The watcher had written down what process-compose
ran, so the next hydra command there deals with it: `hydra up -D` stops them
before it starts anything, `hydra down` stops them and a worktree's own
database, `hydra down --worktrees` does that for every worktree, and a
worktree removed in that state is cleaned up after like any other.

```
hydra: stopped what an earlier run of this stack left running: api (pid 4820), web (pid 4821), worker (pid 4822)
```

hydra goes by the pids it wrote down: a process counts only if it has no
parent any more and was started before the note was written, which makes it
the one the note names. For a stack nobody kept a note for (a main checkout
without a routed service has no router) it goes by the port instead: the
process listens on a port of a service of this slot, carries the
`HYDRA_SLOT` and `HYDRA_SERVICE` hydra started that service with, has no
parent any more, and runs from this checkout. Nothing else is touched. If
process-compose was started some other way and ran into a leftover,
`hydra status` names it, and `hydra process restart` stops it first:

```
✗ web here exited with code 1. Its last output:
      | listen tcp 127.0.0.1:3200: bind: address already in use
    Port 3200 is held by node (pid 4821), left over from an earlier run of this stack.
  fix: hydra process restart web   (stops the leftover first)
```

### Stacked branches

```
main ← b1 (api) ← b2 (worker) ← b3 (web)      each in its own worktree
```

Running `hydra up` in b3's worktree:

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
  b1 and b2 don't need to run `hydra up` themselves.
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

That creates the namespace before the worker starts (with main's
namespace's search attributes), hands it to every upstream pointing at the
worker, and switches it per request on routed services. The namespace is
named after the slot, and a slot goes to another worktree once its first
one is removed, so hydra labels each namespace with the worktree it was
made for and replaces one that was another worktree's: workflows the old
worktree left running never run with the new branch's code.

```yaml
supabase:
  x-hydra:
    kind: supabase        # a worktree whose schema differs gets its own Supabase
    ports: { db: 54322 }  # reached as {port.db}; the API is on port (54321)
    worktree_port: 40321  # worktrees' stacks below 49152 (slot N: 40321 + (N-1) × stride)
    paths: ["supabase/migrations/**", "supabase/seed/**", "supabase/config.toml"]
```

The main checkout runs its Supabase as written. A worktree that changed its
schema files (a migration, a seed, the config) gets a Supabase CLI stack of
its own (stack mode) on its slot's ports, migrated and seeded from its
files, and everything that reads that database runs in the worktree too,
starting only once the database has every migration (a `hydra-ready-…`
step ends then; a migration that fails later shows in `hydra status`).
Every other worktree shares main's, and one that is behind main's migrations
is told to update, like for any other service. With `command: supabase start
--runtime native || supabase start` (and `baseline_command: supabase start`
for main) a worktree's stack runs as plain processes instead of Docker
containers, free of Docker's VM memory cap; it starts in about 20-40 seconds
and takes about 500-650 MB. An idle stack takes about
110 MB. It keeps its data across `hydra down` and `hydra up`, and is
destroyed when the worktree is removed.

The Supabase CLI keeps a stack per directory and branch, so after a branch
is renamed or switched its own commands no longer find the running stack.
Hydra goes by the directory: the services keep using it, migrations still
apply, `hydra down` still stops it, and `hydra status` notes it until
`hydra process restart supabase` starts the current branch's.

Each start of a stack-mode Supabase appends the CLI's lock files to the
tracked `supabase/.gitignore`. Hydra puts the file back when those lines
are all that changed, and ignores the files through `.git/info/exclude`:
in a worktree after each start, in main from its `hydra-router` and in
`hydra update` before it checks that main is clean.

### HTTPS in front of a service

Some programs only take `https` addresses: a production build whose security
policy lets the browser connect to nothing else, or a client that turns TLS on
whenever it is given an API key. Local services speak plain HTTP. `tls` on a
service has hydra's router accept TLS on another port and pass the bytes on to
the service:

```yaml
supabase:
  x-hydra:
    port: 54321
    tls: 54421                         # https://127.0.0.1:54421 goes to :54321
temporal:
  x-hydra:
    shared: true
    port: 7233
    tls: { port: 7243, alpn: h2 }      # a gRPC server: HTTP/2 inside
web:
  x-hydra:
    upstreams:
      NEXT_PUBLIC_SUPABASE_URL: { service: supabase, baseline_value: "https://{host}:{tls}" }
    baseline_command: pnpm build && exec pnpm start
```

- The front belongs to the main checkout's copy of the service, and
  `baseline_value` hands its address to main's process alone. Worktrees keep
  the plain address, of main's copy or of their own. This is what lets the
  main checkout run a production build of a frontend (a third of the memory,
  no compiling on the first request) while worktrees run dev servers.
- hydra makes a local CA and a certificate signed by it, once per user. The
  CA can only vouch for `localhost`, names under it and loopback addresses.
  `hydra trust` makes this machine accept it; until then `hydra status` says
  a browser will refuse the fronts. A process of main that is given a front's
  address gets the CA as `NODE_EXTRA_CA_CERTS`. A stack can bring its own files instead:
  `x-hydra: { tls: { cert: ..., key: ..., ca: ... } }`.
- The front doesn't read what passes through, so it carries what the service
  speaks in the clear: HTTP/1.1 and its websockets by default, HTTP/2 with
  `alpn: h2`.

`./examples/demo/tls.sh` shows it.

See [docs/design.md](docs/design.md) for the `x-hydra` fields and the rules
in full.

### Many worktrees at once

A worktree costs one copy of each service it changed, plus about 60 MB for
its process-compose and watcher. Two things run out before memory:

- **Builds started together.** Wrap a service's build step so builds take
  turns across every stack on the machine:
  `hydra throttle go-build 2 -- go build -o bin/api ./cmd/api && exec ./bin/api`.
- **Main's database connections.** Each worktree's copy of a service with a
  connection pool opens it on main's Postgres (100 connections by default).
  `hydra status` warns from 75% and prints the command that raises the limit,
  which is enough for about twenty worktrees that run their own API. For
  more, one line on the database's service puts PgBouncer in front of it,
  once, in main, and every worktree connects through it:

  ```yaml
  supabase:
    x-hydra:
      kind: supabase
      pool: 6432 # brew install pgbouncer first
  ```

  Main connects directly, and so does a worktree with a database of its own.
  What holds a connection between transactions (`LISTEN`, session advisory
  locks, a `SET` without `LOCAL`) doesn't work through it.

## The demos

`examples/demo` is a mock stack; `./examples/demo/run.sh` starts main's
stack and one worktree and shows which copy each request went through:

```
web-a main     : web-a(slot 0, :20000) -> api(slot 0, :20010) -> db(slot 0, :20030)
web-b main     : web-b(slot 0, :20001) -> api(slot 0, :20010) -> db(slot 0, :20030)
web-b worktree : web-b(slot 1, :20101) -> api(slot 1, :20110) -> db(slot 0, :20030)
```

`./examples/demo/stack.sh` builds a throwaway stack of worktrees and shows
the watcher starting and stopping services as files change in them.
`./examples/demo/routed.sh` shows one frontend serving every worktree, and
`./examples/demo/tls.sh` a TLS front, and `./examples/demo/dashboard.sh`
drives the dashboard in tmux. `./examples/demo/update.sh` is the
end-to-end test: `hydra status`, the checks before `hydra up`,
`hydra update`, a second start, the order in which an edit's services
start, a removed worktree, a killed process-compose and
`hydra down --worktrees`, checking what every step printed. Each demo builds hydra
into its own throwaway directory, so none of them changes the `hydra` on
your PATH.

## Open questions

- Database: a worktree's own Supabase starts from the seed files, not from
  main's data. Copying main's data in would need a dump per start.
- Ports are per repository: two repositories whose stacks use the same
  ports can't run at once.
- Deriving `paths` from the nx / Go dependency graph instead of by hand.
  Until then, the report of changed files nobody lists is the safety net.
- Nothing stops an idle worktree's stack: it runs until `hydra down`,
  `hydra down --worktrees` or the worktree's removal.
