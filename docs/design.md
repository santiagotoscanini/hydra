# Design

The rules, and the decisions behind them. The manual for someone using hydra
is <https://hydra.toscanini.me/docs/> (`site/docs/index.html`).

A word on words: the **baseline** is the main checkout's stack. The config
keys and flags use that name (`baseline_command`, `--route web.api=baseline`)
and so do these notes; everything hydra prints says `main`.

## Usage

`hydra` stands in for `process-compose`; arguments pass through. The one
exception is bare `hydra` at a terminal (see [The dashboard](#the-dashboard)).

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
- `hydra down` runs process-compose's `down` and watches the stack over its
  API while it stops. process-compose waits for ever on a process that
  takes the signal and never exits, unless it has a `shutdown.timeout_seconds`;
  so hydra says every 10 s what it is still waiting for, and SIGKILLs the
  process group of one that has been stopping for 60 s (or its own timeout
  plus 10 s), says so, and journals a `killed` event. `--worktrees` does
  the same for each worktree, and names what it killed on that worktree's
  line.
- `hydra down` on a stack that isn't running says so and exits 0.
  `hydra down --worktrees` stops every worktree's stack and leaves the main
  checkout's. An idle stack stops by itself only with `idle_stop` (see
  [Idle worktrees](#idle-worktrees)).
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

A plan costs about a second of CPU in a large repository, so twenty
watchers re-planning every 2 seconds kept several cores busy with nothing
changing. (Finding a stacked parent took an ancestry check against every
other worktree's branch, three git processes per pair; one `git
for-each-ref --merged=HEAD --no-merged=<fork point>` now names the only
branches that can be a parent.) A tick first computes a key: `git
status` of the worktree, `git
worktree list`, every branch's commit, and the size and mtime of the stack
file, `.hydra.local.yaml`, `hydra-slots.json` and `hydra-baseline.json`. It
plans only when the key moved, and at least every 30 seconds for what the
key leaves out (a stacked parent's uncommitted edits). The note about main's
stack being down, and migrations of a worktree's own database, still run
every tick.

That `git status` was itself the next cost: without a file-system monitor
it reads every tracked file's metadata, 0.3 s of kernel time in the
monorepo, every 2 seconds in every worktree (about three cores for twenty).
Hydra's git calls that read a working tree (`gitx.WorkTree`) pass
`-c core.fsmonitor=true -c core.untrackedCache=true` on macOS and Windows,
where git has a monitor: a daemon per worktree (about 6 MB) answers from
the OS's file events, 0.01 s a call. Passed per call rather than set in the
repository, so nobody's git configuration changes; the daemon stops when
its worktree is removed.

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
| This worktree's stack | a process that should run here failed, was skipped (the step it waited for is shown) or isn't running; `hydra-watch` is down. A process whose output says its port was taken comes with the program listening there (`lsof`): the service's own earlier copy is a leftover, which the suggested restart stops first; another program in this checkout is named with the `kill` that ends it; anyone else's is only named |
| Routed services | the router doesn't answer on the worktree's port, or main's copy doesn't pass the worktree's tag on (`/__hydra/status` on that port) |

Also an error: a migration that didn't apply to a worktree's own running
database (what the watcher kept of the CLI's output is shown), and, after
`--wait` ran out of time (`--timeout`, 10 minutes by default), whatever is
still starting: a wait that gives up is not "ok". A process that was ready
since it last started and whose probe fails now isn't starting: it is an
error, "stopped answering" (process-compose keeps `process_ready_time`). A
native Supabase stack whose own processes were killed looks like that: its
start command exited long ago, so process-compose still says "Launched",
and it had been reported as starting forever. `--wait` reads git and
plans once every 10 seconds and only asks the two process-compose servers
in between (a full look costs about a second of CPU in the monorepo, which
made a ten-minute wait cost six minutes of it); before it reports "settled"
it looks at everything once more, so an edit made a moment ago isn't missed.

Notes: a process that is still starting (`--wait` waits for these), main's
stack file or hydra being newer than the running stack, the main checkout
on another branch or with uncommitted changes, plan warnings, changed
files that no service's `paths` (nor `ignore`) cover, a worktree's own
Supabase that was started for another branch than the one checked out now,
(in the main checkout) a removed worktree whose stack still answers, and a
port of the checkout's stack held by a program that is part of no stack.

That last one (`strays.go`): for each port of a service the checkout is
meant to run (main: every one that isn't off; a worktree: what runs there,
on its slot, once it has one), who listens (one `lsof` of the machine's
listeners), and whether that program descends from a process-compose (one
`ps`, walking the parent chain). The stack's own processes, and another
stack's, do; a server that outlived a removed worktree, or one started by
hand, doesn't. It is named with its pid, program and working directory,
"which no longer exists (a worktree that was removed)" when that is gone,
and the fix is `kill PID`. A note, not an error: the stack may run around
it, and `hydra up` doesn't refuse on it. Left out: `is_daemon` services and
Supabase stacks (they detach from process-compose on purpose) and ports a
container runtime's forwarder holds (Docker, OrbStack, Lima, Colima,
gvproxy). Both commands together take about 0.1 s.

"What this worktree takes from main" is: the upstream targets and
`depends_on` entries of its local, enabled services that don't run here, and
every routed service.

`--all`, or `hydra status` in the main checkout, prints main's processes and
one line per worktree without taking a slot for any of them.

### The snapshot: `hydra status --json`

`hydra status --json` is every checkout of the repository as one JSON
object (`cmd/hydra/snapshot.go`): main's processes and problems, and per
worktree what it runs here and takes from main, its problems with their
fixes, how far it is behind and ahead of main, the branches it is stacked
on, and what `hydra update` would do to it (`git merge-tree` tries the
merge). A problem whose fix is a hydra command carries it as `run`, the
command to run in that checkout; advice has none. `version` changes only
when a field changes meaning or goes away.

`--watch` prints one per line, every second, and reads `refresh [ROOT]`
lines on stdin. What it costs to know differs a lot, so each part has its
own clock:

- Process states: every second, from the process-compose servers. Memory
  (each process with its children: process-compose reports the process
  alone, which for a wrapped command is the wrapper): every 5 seconds.
- A worktree's plan, which walks its working tree for untracked files:
  every 30 seconds while its stack runs, every 2 minutes otherwise, and at
  once after a commit in it or in main.
- Where it stands against main (behind, ahead, the trial merge): when its
  HEAD, main's or its parent's moves; when its index file or MERGE_HEAD
  changed (files staged, a merge begun, resolved or aborted: two stats per
  worktree a second), at most every 3 seconds; and otherwise every 2
  minutes, for uncommitted work, which keeps an update away.
- The slow parts due for worktrees that moved run first. After main moves,
  every worktree is due at once, and the one an agent just merged must not
  wait behind twenty that only need a look.
- Which worktrees there are: every second (`git worktree list`).
- Programs outside any stack holding a stack's ports (`strays`, with a
  problem each): every 15 seconds, one `lsof` and one `ps` for all
  checkouts together.

Slow parts run in the background, four at a time, and never hold up a
snapshot: they show up in the next. Git runs with `GIT_OPTIONAL_LOCKS=0`,
so polling never takes `index.lock` from someone's commit. With 30
worktrees, 6 of them up, a watch takes about a tenth of one core. A
worktree that never ran has no slot (0), so it shows what it would run
without ports. Nothing in it takes a slot.

Links, in the snapshot's `url` and in what `hydra up` prints, name
`localhost` where services are wired to each other by `127.0.0.1`. A dev
server treats a page opened by IP address as another origin: Next.js then
refuses it the hot-reload socket and its dev resources.

### The dashboard

Bare `hydra`, when stdin and stdout are terminals and the stack file has
an `x-hydra` block, opens a dashboard of main and every worktree. Anything
else (an agent's shell, a script, a pipe, any argument) gets what bare
`hydra` always was, process-compose with no arguments, so nothing that
scripts hydra changes. process-compose's own TUI on a stack is `hydra up`,
or `hydra attach` on one that runs.

Three layers, each doing one thing:

- **Go** knows everything: the snapshot above.
- **The dashboard** only draws `hydra status --json --watch` and runs
  hydra commands. It is an Ink (React) app in `tui/`, bundled into one file
  that the binary embeds (`cmd/hydra/dashboard.mjs`, committed, so `go
  build` needs no npm), written to the user's cache under a name that
  changes with its contents, and run with node (20+; hydra says so when
  node is missing or older).
- **Every key is the command an agent would type**, in the checkout it is
  about: `u` `hydra up -D`, `d` `hydra down`, `r` `hydra process restart
  NAME`, `l` `hydra process logs NAME -f`, `f` a problem's `run`, `m`
  `hydra update --here`, `U` `hydra update`, `⏎` `hydra attach`. (`o`
  opens an address by name, `hosts`, in the browser: what a person
  types into one.) So the
  dashboard can do nothing a person or an agent can't do without it, and a
  fix to a command reaches both. What stops a stack, runs a fix or moves a
  branch asks first; `m` and `U` show their dry run as the question. After
  a command, the dashboard asks the watch to read that checkout again.

Opening a stack hands the terminal to `hydra attach` and takes it back
when that ends. In process-compose's attach mode, F10 or ctrl-c asks
"Detach from the remote project?", and Yes ends the attach and leaves the
stack running: that is the way back, and the help and the title say so.
(`q` does nothing there.) Commands that only print run in the background,
with their last line as the result and their output shown when they fail.

Making and removing worktrees is not hydra's job: the tool that runs the
agents does it, and the dashboard shows a new worktree within a second and
a removed one goes, cleaned up as any removed worktree is.

It replaced a local web page (`hydra ui`) that served the same facts. The
page was a second program to keep up with every fact (an HTML file and a
JSON endpoint), it only read, and it needed a browser beside the terminal
where the work happens. Acting on what it showed meant copying a command
into a shell. The dashboard sits where the commands run, and the JSON it
reads is the contract scripts and agents get too.

### For agents: `hydra mcp`

Agents drive most of the stacks, and given a shell they run the wrong
command often enough to cost the developer an afternoon. They start
`process-compose` by hand in a worktree, which runs everything from the
plain file on main's ports. They `hydra down` in the main checkout because
their own stack "looks broken". They `supabase stop` or `supabase db reset`
the database every worktree shares. Each one is a command the agent was
allowed to type. A rule in an `AGENTS.md` lowers the odds, and it isn't a
guarantee.

`hydra mcp` gives them the same commands as MCP tools over stdio:
`status`, `up`, `wait`, `logs`, `restart`, `down`, `update_here`,
`main_status`, and for updating everything `worktrees`, `update_main`,
`update_worktree`. The harness starts it once per session in the agent's
worktree, and it serves the checkout its working directory is in, as any
hydra command does. Rules:

- **What changes a stack never changes main's.** `up`, `down`, `restart`
  and `update_here` refuse in the main checkout and tell the agent to ask
  the developer. Reading main (`main_status`, `logs` with `main: true`) is
  allowed: an agent that can say why main is down helps more than one that
  can't. With the tools in place, a project can deny the shell commands
  (`process-compose`, `hydra down`, `hydra update`, `supabase stop`,
  `supabase db reset`). A shell rule can't tell which checkout a command
  runs in. The tool can.
- **The actions run hydra itself** (`hydra up -D`, `hydra down`,
  `hydra process restart`, `hydra update --here`) as a child, so they
  behave exactly as the commands do, checks before a start included. Their
  output comes back verbatim, and a failure is the tool's error. The child
  writes to a file, not a pipe: a process-compose started with `-D` keeps
  what it inherited open, and the read of a pipe would only end with the
  stack. One action runs at a time.
- **The reads call the code behind `hydra status` and its snapshot**, for
  this checkout only, so the problems and fixes are the texts of every
  other surface. `wait` uses `hydra status --wait`'s rule: a process that
  stays failed for three polls in a row has failed. One that is between
  restarts looks failed for a moment.
- **Updating everything is a prompt, not a tool.** Bringing main and
  every worktree up to date means resolving the conflicts `hydra update`
  leaves alone, which takes judgement: read both sides, run the checks,
  ask when it is ambiguous. hydra can't do that and shouldn't try. It
  gives the agent the facts (`worktrees`: every worktree in the order to
  update it, a branch after the one it is stacked on, with every file a
  merge would conflict in, worked out with `git merge-tree` in memory) and
  the two steps it can do safely (`update_main`, which is
  `hydra update --main-only`, and `update_worktree`, which is
  `hydra update --here` run in another worktree). The workflow that ties
  them together and says how to resolve a conflict is an MCP prompt,
  `update_all`, shipped in the binary: registered once at user scope, it
  is there in every repository with no files to copy, and it always
  matches the tools it calls. These two tools work from the main checkout
  too: they change other checkouts than the server's, and `update_main`
  changes main's only when the developer asked, which the description
  says and an ask rule in the agent's settings can enforce. After a merge
  made by hand, `hydra update --here` has nothing to merge and so
  restarts nothing; `update_worktree` takes `since`, the commit before
  the merge, and restarts what changed since, with the code `hydra
  update` uses.
- **stdout is the protocol's.** Much of hydra prints to stdout, so while
  the server runs `os.Stdout` is stderr and the messages go to the stdout
  the process started with. The tests and `examples/demo/mcp.sh` check
  that every line on stdout is a JSON-RPC message.
- **Hand-written, no SDK.** The part of MCP a tools-only server needs is
  `initialize`, `tools/list`, `tools/call`, `ping` and a few
  notifications, one JSON object per line: about 200 lines. An SDK would
  bring more dependencies than hydra has.

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

**Far behind.** In step or not, a branch that stays behind gets harder to
merge. Past `behind_warn` commits behind M (100), or as soon as merging M
would conflict, `hydra status` warns, naming the files. The conflict comes
from `git merge-tree --write-tree --name-only HEAD M` (git 2.38), which
merges in memory and touches neither the worktree nor its index; it exits 1
on a conflict, but also on a commit it can't read, so only output that
starts with a tree counts as an answer. On a large repository it takes
seconds, so its answer is cached by the pair of commits, which it can never
change for: the snapshot works it out on its slow git tier and keeps it
while neither commit moves (its two-minute look for uncommitted work reuses
it). `hydra update`'s own merge-tree is skipped for a worktree with
uncommitted work, which every agent's has, so this one is separate. It is a
warning, not a refusal: the stack runs fine on a branch that is merely old,
and `hydra up` doesn't pay for the merge.

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
   branch is already in main, squash-merged for instance). merge-tree exits
   1 on a conflict and also on a commit it can't read, so only output that
   starts with a tree is a conflict; anything else leaves the worktree alone
   with git's error, never as "CONFLICTS in 0 files". After a merge in
   a worktree whose stack runs, the processes whose `paths` match the merged
   files are restarted there, in the same way as in main.

`--dry-run` prints all of this without doing it, `--main-only` skips step 3,
`--no-pull` step 1. `--here`, in a worktree, is step 3 for that worktree
alone: main isn't pulled and its stack is left alone, for when one branch
needs main's latest without moving anyone else's. One update runs at a time per repository (a lock in the
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

Commands of its own: bare `hydra` (the dashboard), `hydra status` (is it
all up and in step; `--json` for every checkout as data), `hydra update`
(move main and bring everything along; `--here` merges main into one
worktree only), `hydra plan` (what runs where, and why), `hydra render` (the
generated file), `hydra slots` (which worktree has which slot, and freeing
one), `hydra throttle` (machine-wide turns for a build step), `hydra trust` (the CA behind TLS fronts) and `hydra version` (the
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
  idle_stop: 2h           # stop a worktree's stack nobody used for 2h (default off; see Idle worktrees)
  behind_warn: 100        # warn past this many commits behind main (default 100)

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
      hosts: 3900                 # http://<worktree>.localhost:3900, main's at localhost:3900 (see Hosts)
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
      worktree_port: 40321        # worktrees' ports start here, out of macOS's ephemeral range
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
- The main checkout's Supabase isn't wrapped (the stack file's
  `baseline_command` opens it by `--stack-id`), and a stack-mode start
  appends to `supabase/.gitignore` there as well: a main with changes stops
  `hydra update`. So main gets the same tidy at two moments: main's
  `hydra-router` stats the file every two seconds and tidies when it
  changed (and once when it starts), and `hydra update` tidies before it
  checks that main is clean (`--dry-run` only says it would). A main
  without a router gets the second only.
- The shutdown command exports the same environment itself (a `supabase
  stop` without it stops the baseline's Supabase). The stack file's command
  runs between `hydra prepare supabase --stopping`, which skips it when the
  directory is no longer a worktree (see Removed worktrees), and
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

**Main's data, with `seed_from_main: true`.** Seeds are a few rows, so
agents in a worktree test against an almost empty app, and a migration that
breaks on real rows (a `NOT NULL` column without a default, a unique index
over duplicates) passes there and fails after the merge, on main. With the
key, a worktree's database is main's state plus the branch's new
migrations:

- `--before` decides whether this start creates the stack: no stack of this
  directory has the current branch (`supabase stack list`), or `hydra db
  reseed` left `.process-compose/supabase-reseed`, in which case it first
  destroys the branch's stack by id. Then it leaves
  `.process-compose/supabase-seed-from-main`, and the rendered command
  exports `SUPABASE_DB_MIGRATIONS_ENABLED=false SUPABASE_DB_SEED_ENABLED=false`
  for that start: the CLI makes the stack with Supabase's own schemas and
  nothing of the app's.
- `--started` (the start worked, the file is there) reads main's applied
  versions from `supabase_migrations.schema_migrations`, applies those files
  from the worktree with `supabase migration up --include-all --workdir
  <temp dir holding only them>` (as `postgres`, who has to own what the
  branch's migrations alter), copies the rows, removes the file, and then
  runs the usual `migration up`, which now applies only the branch's own
  migrations, on main's rows. A failure there fails the start, with the
  CLI's `Applying migration …` and Postgres's error as its last output, and
  the data stays: the corrected file applies on the next restart.
- The copy is pgx `COPY … TO STDOUT` from main piped into `COPY … FROM
  STDIN`, table by table, in the text format (the binary one carries type
  ids that differ between servers), inside one repeatable-read transaction
  on main and one transaction on the worktree, as `supabase_admin` with
  `session_replication_role = replica` (no triggers, no foreign key checks:
  the rows were consistent in main; a trigger on `auth.users` that inserts a
  profile would otherwise double them). It truncates the target tables
  first (migrations insert rows too), skips generated columns, partitioned
  parents, unlogged tables and tables an extension owns, and sets every
  sequence to main's value. Supabase's own schemas (`realtime`, `vault`,
  `cron`, `net`, `supabase_functions`, `extensions`, …) and the services'
  migration records (`auth.schema_migrations`, `storage.migrations`) are
  left out: the worktree's stack has its own, and what migrations put there
  they put there again. Auth and storage are copied: users, identities,
  buckets and objects' metadata (the files themselves aren't).
- Measured on CLI 2.122.1, native stacks, Postgres 17: 1,001,010 rows in
  37 tables (91 MB as text) in 2 s, main's migrations included; `pg_dump
  --data-only | psql` with the CLI's own 17.11 binaries takes 1.8 s for the
  one large table. COPY needs no `pg_dump` that matches the server's
  version (the CLI's cache holds 15 and 17, a PATH may hold anything) and
  writes nothing to disk. A whole reseed, destroy and create included,
  takes about 10 s.
- It refuses rather than guesses: main's database not answering, main
  having migrations the branch lacks (its rows wouldn't fit; merge main
  first), the worktree's database having migrations main hasn't, and both
  addresses reaching one server (`system_identifier`), which a stack file
  that exports main's ports in the shared `command` does. Each fails the
  start with one line saying why, and the file stays, so the next start
  copies. No fallback to the seeds: an empty database where main's data was
  asked for is the surprise this exists to remove.
- Once only. A resumed stack keeps its data, and `hydra db reseed` is the
  way to copy again: it leaves the request, restarts the process through
  process-compose's API and waits for `.process-compose/supabase-branch`,
  printing what the copy did (`.process-compose/supabase-seeded`).

New kinds (SQS queues, Redis key prefixes) go in the same table.

The runtime is the stack file's choice, not hydra's: `command: supabase
start --runtime native || supabase start` with `baseline_command: supabase
start`. Native runs the stack's services as processes on the host (macOS 14+
on Apple silicon, Linux), so worktree databases aren't bound by Docker's VM
(8 GiB on Colima, about six stacks next to main's). Measured in the
monorepo: first start 39 s against Docker's 81, a second stack 21 s, resume
1 s, 500-650 MB a stack once seeding is done; stop, resume with data, and
`stack destroy` by id behave as with Docker, and `stack list` reports the
same `project_root`, so the reap needs nothing new. The CLI fixes a stack's
runtime at creation and refuses another later, hence the fallback, which
resumes an existing Docker stack.

Main can run natively too, still from the stack file alone, as it has in
the monorepo since 2026-11-24. The CLI keys a stack on folder and branch
(`--stack NAME` included: tried, and a start on another branch made a new
Docker record), so main's stack is opened by `--stack-id`, looked up by
folder and name. That keeps one database whatever main has checked out, as
Docker had. The data moved with `pg_dump` and a data-only `pg_restore` into
an unseeded stack; all 565 tables had equal row counts. `hydra status`'s
connections fix picks Docker or native by asking Docker who publishes the
port.

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
  It reads the stack main's processes were rendered from, a copy kept in
  `.process-compose/hydra-source.yaml` (written on `hydra up` in main and
  after a `hydra update` reload), not the file itself: those ports sit one
  stride past `max_slots`, so raising it in the file before `hydra update`
  would move the router while main's web still calls the old ports, and
  hand those to the worktree on the next slot.
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

## Hosts

A worktree's ports move with its slot: the slot it gets on the next start
may not be today's. `hosts` on a service is a port where the router serves
every checkout's copy by name, `http://<worktree>.localhost:<hosts>`, and
main's at `localhost` (or `main.localhost`):

```yaml
web:
  x-hydra: { port: 3000, hosts: 3900 }
```

- **Which copy.** The router reads the name from the `Host` header and
  answers for that worktree the way the worktree's own wiring would: its
  own copy when it runs the service (on whatever slot), main's copy through
  the worktree's routed entry when the service is routed (tag, per-request
  values, the health check: the entry's handler itself), and main's copy
  untagged otherwise, which is what the worktree takes from main. A name no
  running worktree has is a 404 that lists the running ones, and a worktree
  whose own copy isn't accepting connections a 503: never main's page under
  a worktree's name, for the reason routed calls never fall back. A Host
  that isn't `localhost` or a name under it is a 404 too.
- **The name** is the worktree folder's (`config.HostLabel`): lowercased,
  every character outside `[a-z0-9]` a hyphen, trimmed, at most 63 (a DNS
  label). The branch was the other candidate; it is renamed and switched in
  place, and the address would move with it, while a folder stays for the
  worktree's life. The worktree puts it in its routes file. Two running
  worktrees with one name (folders of the same name in different
  directories) are a 409 naming both, rather than a guess.
- **Its own port, like a TLS front.** The router never takes main's own
  ports. Serving names on 3000 would mean moving main's process to an inner
  port: a change to main's render for every service with it, a hop through
  the router for every request to main, and main's web gone whenever the
  router is. A separate port leaves main's ports and wiring alone. It is
  opt-in all the same, because it takes a port and changes one process's
  environment (below).
- **The Host header goes through unchanged**, so the app builds links for
  the address the browser is on and a dev server's origin checks see it. A
  `Location` that points at the copy's own port is rewritten to the name.
  The proxy is `httputil.ReverseProxy`, which carries an `Upgrade` request
  (a dev server's hot-reload websocket) through: `TestHostsPortPassesAWebsocketUpgrade`
  checks that the 101 and both directions after it arrive. Next.js allows
  `**.localhost` for its dev resources by default (`blockCrossSiteDEV`,
  checked on 16.4), Vite allows `.localhost` hosts too.
- **Resolution** is the client's. RFC 6761 reserves `localhost`; browsers
  and curl answer `*.localhost` with loopback themselves, and macOS's
  resolver does for every program that asks it (Node's `dns.lookup` and
  `fetch`, Go's default resolver on macOS: `::1` then `127.0.0.1`, which is
  why the router listens on both families). Go's pure resolver (`netgo`,
  `CGO_ENABLED=0`) and Node's `dns.resolve*` query DNS and fail. So the names
  are for people and browsers; services keep wiring by port.
- **Cookies and sign-in.** Cookies are per host name, so each name has its
  own jar, where every slot port shares `localhost`'s. An auth server
  redirects back only to addresses it knows: the project adds the pattern
  (`additional_redirect_urls = ["http://*.localhost:3900/**"]` for Supabase).
  hydra changes no app code or project file. The service's own process gets
  `HYDRA_URL_<SERVICE>` with its address in that checkout (main's copy of a
  routed service has main's, also while serving a worktree, so it should
  use the request's Host there). Only that process: other processes'
  environments stay as they were, so adding the key restarts one process in
  main on the next `hydra update`.
- **`hydra status`** prints an `address` line per checkout, and notes (`!`)
  when the router isn't the one answering on the port. `hydra up` prints
  the worktree's address, `status --json` has `address` on each service,
  and the dashboard shows it and opens it (`o`). A request at a worktree's
  name is activity, for `idle_stop`.

## Slots

`hydra-slots.json` in the shared `.git` directory maps worktree paths to
slots, guarded by flock. A worktree keeps its slot across runs. Ports for
every slot up to `max_slots` are checked against each other and
`reserved_ports` when the config loads. They are computed, not probed:
another program may sit on one, which `hydra status` then names.

A slot stays taken until it is released. An entry whose worktree is gone
is not dropped on sight, as it once was: whatever that worktree ran may
still hold the slot's ports.

`worktree_port` on a service moves its worktree ports to another base:
slot N listens on `worktree_port + (N-1)*slot_stride`, named ports and a
Supabase stack's own with it (one function, `Stack.Shift`, computes every
port). From Supabase's 54321 with a stride of 200, slot 4 and up reach
49152, macOS's ephemeral range, where any outgoing connection may hold the
port at the moment a stack binds it.

A slot where any service's port is one of the Fetch standard's "bad ports"
(6000, 6665-6669, 10080, ...) is skipped when slots are handed out: browsers
refuse those ports and Next.js won't listen on them, and the monorepo's web
on 3000 with a stride of 200 reaches 6000 in slot 15. Skipping beats failing
the config: the stride is often set by habit across a team, and losing two
slots of twenty costs less than renumbering every port.

Nor is a slot handed out to a new holder while something already answers on
one of its ports (a 200 ms connect to each, on `x-hydra.host`). The program
may be the system's own: macOS's AirPlay Receiver listens on 5000 and 7000,
and the monorepo's web reaches 7000 in slot 20. A worktree that already
holds the slot keeps it, since what answers is usually its own stack; when
it isn't, `hydra status` names the program and the fix is
`hydra slots free && hydra up -D`, which starts the worktree on a slot
where nothing listens. Main can't move, so for slot 0 the fix is to stop
the program.

Only a start takes a slot. `hydra plan`, `render`, `status` and the page
peek: the slot the worktree has, or the lowest free one, recorded nowhere.
Once `plan` took one, and worktrees that were planned but never run filled
the registry.

When every slot is taken, a start takes the slot of a worktree whose stack
isn't running (its process-compose doesn't answer), the one whose generated
file is oldest, and prints that it did. A removed worktree's slot is never
taken this way: freeing it is the reap's, which first stops what still holds
its ports. So `max_slots` bounds stacks that run at once, not worktrees that
ever ran. Nothing keyed by the slot number goes with it to the new owner:
the router's routes file for the slot is removed; a Temporal namespace is
labelled with its worktree and replaced; a worktree's own Supabase stack is
named by the Supabase CLI after folder and branch, so the old owner gets
its data back when it starts again, on its new slot's ports. Should
something of the old owner still hold a port, the checks before the start
name it.

## Many at once

Two limits come before memory, both met in the monorepo with seven
worktrees started together:

- **Builds.** Each Go service builds before it runs. Fourteen builds at once
  (api and worker in seven worktrees) put the load at 450 on 18 cores and
  froze the machine for half a minute. `hydra throttle NAME N -- CMD` lets N
  commands of a name run at once machine-wide, each holding a flock on one
  of N files in `$TMPDIR`, polled every 200 ms. A lock goes with its process,
  however that ends, so no turn is ever lost to a crash. Only the build is
  wrapped; the service runs outside it. It is a command for the stack file
  to use, not something hydra does by itself: hydra can't tell the build in
  a command from the run.
- **Connections to main's database.** Each copy of a Go service holds a pool
  of 10 to 30 connections; Postgres allows 100 and Supabase holds about 34.
  Main and two or three worktrees fill it, and the next copy fails with
  SQLSTATE 53300, which reads like a bug in the service. `hydra status`
  asks main's Postgres (`pg_stat_activity`, 2 s timeout) and warns from 75%.
  Its fix raises `max_connections` to 300 (by 200 when it is there already) as `supabase_admin` (the
  `postgres` role may not) and restarts the database: in Docker the container
  together with Supabase auth (auth keeps its dead pool across a database
  restart and fails every login until it is restarted), natively the whole
  stack through `hydra process restart supabase`.
- **A pool for many worktrees.** Raising the limit holds about twenty
  worktrees that run a Go API and worker (about 20 connections each, measured).
  Smaller pools per worktree would hold more, but apps often hardcode their pool
  sizes, and changing an app to suit a laptop is the wrong trade. So the
  option is infrastructure: `pool: PORT` on the database's service adds one
  `hydra-pgbouncer` process to main, and every worktree upstream that reaches
  main's copy through the port it stands in front of (`{port.db}` for a
  Supabase kind, `{port}` otherwise) gets the pool's port instead. The rules:
  - **One PgBouncer, in main.** Worktrees come and go; the pool is main's,
    like the database, and the number of real connections stays at `size`
    (20) however many worktrees there are.
  - **Only worktrees go through it.** Main's own services and a worktree
    with a database of its own connect directly, so turning the pool on
    changes nothing in main but the process it adds, and a stack that needs
    a session (a `LISTEN`) still has a way to run.
  - **Transaction pooling.** That is what makes the connections shared.
    PgBouncer 1.21+ keeps pgx's prepared statements working
    (`max_prepared_statements`); `LISTEN`, session advisory locks and plain
    `SET` don't survive it, and the docs say so.
  - **No credentials to configure.** PgBouncer trusts its clients, as a
    local Postgres does, and logs in to Postgres with the passwords found in
    the stack's own URLs to that service (plus `postgres`/`postgres`). Its
    files are written from the started-from snapshot each time it starts.
  - **Checked like the router.** `hydra status` dials its port; when it is
    down, a worktree that goes through it is blocked, with the restart (and
    the install when `pgbouncer` is missing) as the fix. Its readiness check
    runs every two seconds and stays out of the journal.

## Removed worktrees

Nothing tells hydra that a worktree was removed: git has no hook for it.
A worktree removed with its stack up used to leave process-compose and its
services running, on ports the freed slot handed to the next worktree, and
its Supabase stack's containers and data for good.

**The folder usually stays.** `git worktree remove` deletes the files and
then forgets the worktree, whatever became of the files. A running stack
writes into the folder meanwhile (logs, build output, caches), and git gives
up at the first directory that isn't empty after it emptied it:
`error: failed to delete '…': Directory not empty`. What is left is one of
two things, depending on where git stopped:

- a folder holding only what was written after git passed, with no `.git`;
- most of the checkout, with a `.git` file naming a git directory that is
  gone.

So "the directory is gone" is the wrong test, and was hydra's first one: the
stack of such a worktree ran on for good. A slot's worktree counts as
removed when its `.git` is missing, or is a file whose `gitdir:` doesn't
exist (`slots.Removed`). That is two `stat`s and a read, cheap enough to
ask four times a second.

**A folder without `.git` inside the main checkout is the dangerous one.**
git answers for the nearest repository above a directory, so every git
command run in what is left of `main/.claude/worktrees/x` speaks for the
main checkout: its root, its branch, and for hydra its plan, which is the
whole stack on main's ports. Three things keep a stack that still runs
there from acting on that:

- the watcher checks for the removal before each look and again before it
  applies anything, and refuses a look in which git names another root than
  the one it was started in;
- `hydra prepare supabase` refuses to run in the main checkout, in every
  mode;
- the rendered stop of a worktree's database runs the stack file's own stop
  command (`supabase stop`, which finds its stack by directory) only after
  `hydra prepare supabase --stopping` has confirmed that the directory is
  still a worktree. Otherwise the stack is stopped by its id.

**What git says decides what is destroyed.** A `.git` that is missing has
two causes that look the same from inside the folder: git is removing the
worktree, or someone deleted the file. Only the first is a removal, and the
second can be mended (`git worktree repair`). So there are three states:

| State | Test | What happens |
|---|---|---|
| present | `.git` is there and leads somewhere | nothing |
| leaving | not present, and `git worktree list` still has it | the stack is stopped; slot, database and files are kept |
| removed | not present, and git no longer lists it | below |

`git worktree remove` forgets the worktree last, so a worktree in the
middle of a removal is `leaving` for as long as git takes to delete it.

Now:

- **The stack stops itself, and the folder goes.** `hydra-watch` checks at
  the top of each tick, and every 250 ms while it sleeps, whether its
  worktree is still one. When it isn't, it starts `hydra reap ROOT` from the
  main checkout, in a session of its own, and exits: the watcher is one of
  the processes the stop ends, so it can't see the job through itself.
  `hydra reap` runs `process-compose down`, waits for git to be done (up to
  five minutes: deleting a large checkout takes a while), then destroys the
  Supabase stacks made for that directory, deletes the folder, removes the
  routes file and the socket, and releases the slot. It deletes all of the
  folder, because its caller saw the worktree removed under a running
  stack: what is left is what git was told to delete and gave up on.
- **The next start finishes the job.** `hydra up` (any checkout, before it
  takes a slot), `hydra update` and `hydra down --worktrees` go through the
  slots of removed worktrees and do the same, with two differences. They
  don't wait for git: a worktree that is `leaving` is looked at again by the
  next start. And from the folder they delete only what hydra wrote
  (`.process-compose/`, its block in `.env.local`, the Supabase CLI's empty
  temporary directory), then the folder if that empties it. A folder found
  later may have another story (a volume that was away while git pruned the
  worktree comes back with every file and a dead `.git`), so anything else
  in it is named, not deleted. A step that fails leaves the slot taken,
  with a warning, and the next start tries again.
- **One at a time.** The watcher's reap and a `hydra up` elsewhere, or two
  of those, may come for the same worktree. An flock next to the worktree's
  socket (`hydra-<hash>.reap`) makes it one process's job: a start that
  finds it taken leaves that worktree alone.
- **The stop comes before the database.** process-compose's own stop of the
  database does nothing in a folder that is no longer a worktree (above), so
  the stacks are dealt with afterwards, by id, which needs no directory.
- **`hydra slots free PATH`** does the same for one worktree, removed or
  not. For one that is still there, or that git still lists, the Supabase
  stack is stopped, not destroyed, and no file is touched.
- **Temporal** needs no cleanup at removal: the namespace is replaced when
  the slot's next worktree prepares it (see Kinds), which also works when
  Temporal was down at the time of the removal.
- **A killed process-compose** is the other way to leave things behind:
  nothing it had started ends with it (next section). A worktree removed in
  that state has no watcher to notice; the reap then stops what the stack
  left running before it frees the slot.

## Idle worktrees

Agents finish and leave their stacks running, a gigabyte or two each. With
`idle_stop: 2h` in `x-hydra`, a worktree's stack that nobody used for that
long stops, the way `hydra down` there would. Unset, `0` or `off`: never.
The main checkout's never stops; everything leans on it.

- **What counts as use.** A request for the worktree through the router, and
  a change to its files. Both are already seen by something hydra runs, so
  neither costs a new watcher. A process restart doesn't count: a service
  that crashes in a loop isn't someone using it.
- **The router tells the worktree.** It is one process for every worktree,
  in main's stack, and knows each request's worktree by its tag. It touches
  `.process-compose/hydra-activity` in that worktree, at most once a second
  per worktree, and only when the folder is there (a worktree being removed
  gets nothing written into it). hydra's own requests (the status path that
  `hydra status` asks, the router's probe, whose tag is nobody's) don't
  count, or a dashboard would keep every stack up.
- **The watcher keeps the clock.** It looks at `git status` every two
  seconds anyway (`watchKey`); the clock reads the same output. A change in
  what it lists is use from that moment; a file it lists that is saved again
  is use at its modification time; so is the router's file. The clock starts
  when the stack does. Only this worktree's status counts: `watchKey` also
  covers every branch of the repository, which moves whenever any agent
  commits, and would keep everything up.
- **Stopping from inside.** The watcher is one of the processes the stop
  ends, so, as for a removed worktree, it starts `hydra stop-idle ROOT AFTER`
  in a session of its own. That stops the stack through its socket, writes
  `.process-compose/idle-stopped` (when, and after how long) and an `idle`
  event to the journal, or the event with the error when the stop failed.
  The note is written only once the stack is down, so it never describes a
  stack that runs. The watcher's next start removes it.
- **Where it shows.** `hydra status` in the worktree says "stopped after 2h
  idle; hydra up starts it again" in place of "isn't running",
  `hydra status --all` says `idle-stopped`, `hydra status --json` has
  `idleStopped`, and the dashboard's STACK column `idle-off`.
- **What it misses.** Requests straight to a service the worktree runs
  itself (its own api on its own port) don't pass the router. Edits go with
  them, usually; a worktree only browsed that way for longer than
  `idle_stop` stops, and `hydra up -D` brings it back with its data.

## What a killed process-compose leaves

process-compose starts each process in a group of its own, so killing it
(`kill -9`, a crash, the out-of-memory killer) ends none of them. In the
monorepo this was written against, all of these outlived it: the api, on its
port; the worker, taking work with the code it was started on; `next dev`,
which is two processes, a child on the port and a parent that holds a lock
on the project; and the idle stand-ins for main's services, which nothing
ever ends. Twenty-one of those had piled up on one machine before anyone
looked.

**First, hydra's own process in the stack stops them**: the watcher in a
worktree, the router in main. Both compare their parent process four times a
second, which also keeps two watchers from updating one project and two
routers from fighting over a port. Both ask process-compose every two
seconds what it runs: the processes it reports as `Running`, which are the
ones it holds as children. A daemon (`Launched`) is left out, because the
pid process-compose has for it is its launcher's, long gone and possibly
somebody else's by now; so are hydra's own steps, which look after
themselves. When the parent is gone, each of those pids that is still there
and has no parent is stopped with everything below it (SIGTERM, SIGKILL
after five seconds), and the watcher or router exits.

Two things about that process are easy to get wrong. It prints into a pipe
that process-compose reads, and the first line it prints after
process-compose died would end it with SIGPIPE before it had stopped
anything; it asks for that signal, which turns the failed write into an
error it ignores. And it must not go by the stack file to tell a daemon: in
a worktree, `supabase` and `containers` are stand-ins, ordinary processes,
though the stack file marks the real ones `is_daemon`.

**Second, the note.** Whatever killed process-compose may have killed the
watcher with it: `pkill -f process-compose` matches both, because the
watcher's command line names the stack file. So what process-compose runs
is also written down, name and pid, in `$TMPDIR/hydra-<hash>.pids` beside
the stack's socket, whenever it changes. It outlives the checkout's folder,
and goes when the stack is stopped the regular way (the watcher is asked to
stop then, and nothing is left running), when a later command has stopped
everything it names, and when the slot is freed. `hydra up` reads it before it starts
anything, `hydra down` when process-compose isn't running (it then also
stops a worktree's own Supabase stack, by id, since only process-compose's
shutdown would have), `hydra down --worktrees` for every worktree, and the
reap of a removed worktree.

A pid from a note may be days old, and a number can belong to another
program by then. The watcher's own list can be as old: it keeps the last
answer of a process-compose that stopped answering. So for either, a pid
counts only if the process

1. has no parent (pid 1, or systemd's user instance where that adopts
   orphans);
2. was started before the list was taken (the note written, or the last
   answer heard). It then held that number at that moment, so it is the
   process the list names;
3. carries this slot's `HYDRA_SLOT` and its service's `HYDRA_SERVICE`,
   where its environment can be read. hydra renders both into every
   process and children inherit them; they are read from
   `/proc/PID/environ`, or from `ps -E` on macOS. A stand-in has none, and
   a program that renames itself (`next-server`) can't be read.

**Third, the port**, for a stack nobody kept a note for: a main checkout
without a routed service has no router. `hydra up`, `hydra down`,
`hydra process restart NAME` and `hydra process start NAME` take a process
tree for the service's own earlier copy when all four hold:

1. one of its processes listens on a port of the service, in this slot
   (one `lsof` for every listener);
2. its top process, found by walking up from the listener, carries the
   marks for this slot and that service. The listener itself is often a
   child whose marks can't be read;
3. that top process has no parent;
4. its working directory is in this checkout, or in the worktree the plan
   runs the service from. Another repository's slot of the same number has
   services of the same names.

**The marks alone are never enough.** They, the missing parent and the
directory also fit what a step of the stack leaves running on purpose. In
the monorepo, main's stack has a `colima` step that starts a Docker VM: its
host agent is orphaned by design, carries `HYDRA_SERVICE=colima` and
`HYDRA_SLOT=0`, and runs from the main checkout. A build tool's daemon
started by a service looks the same. A rule on the marks would have stopped
every container the next time main's stack started. Neither is a pid
process-compose named, and neither listens on a port of the service whose
marks it carries: where the VM does listen (it forwards the database's
port), its marks name another service than the one that owns the port. A
service that does leave something listening on its own port after its
command ends says so with `is_daemon`, and those are skipped.

What this leaves uncovered: a process without a port in a stack without a
note (a worker in a main checkout that has no routed service), and a process
without a port that was started in the two seconds before the kill, with
the watcher killed as well.

With nothing noted and every port free, which is the usual start, the check
is one `lsof` and costs about 50 ms. A note with entries costs one listing
of the processes (about 45 ms with `ps` on macOS), and one more `ps` for
each pid that is still there.

A `prepare` command of the stack file has no counterpart that runs at
removal. What it creates is named after the slot, so it is found again by
the slot's next worktree; a step that must start clean has `HYDRA_ROOT` to
tell whose it was.

## The journal

hydra is used by agents in many worktrees at once, and what goes wrong
there is seen later, by someone else. So every hydra process writes what it
does and sees to one journal per repository: `<git common dir>/hydra-journal/`,
a JSON-lines file per local day, deleted after 30 days.

- **What.** Every command when it ends (arguments, directory, duration, exit
  status, the error it printed); each problem a check showed someone, from
  the one place problems are printed; a worktree taking a slot; a stack
  stopped for being idle; every line
  the watcher and the router print; and the state of the stacks.
- **Process changes cost nothing extra.** The watcher (a worktree) and the
  router (main) already ask their process-compose what runs every two
  seconds, to bury what a killed one leaves. The monitor rides on that
  answer: a process whose status, readiness, restarts, exit code or pid
  changed is written as it is seen, with the last 30 lines of its output
  when it failed or restarted, fetched while process-compose still has them.
- **Usage once a minute** from the same loop: one `ps` for the machine,
  summed over each process's tree (`pnpm` and `go run` wrappers hold their
  servers as children). A native Supabase stack runs detached, under a host
  process of the CLI's named by the stack's id; the CLI says which checkout
  an id belongs to, asked once per id. Resident memory, not macOS's footprint, which takes
  a call per process; it reads high for `next dev`, whose cache files are
  mapped, so the trend is what it is good for. Main's monitor adds the
  machine's free memory and swap, once for everyone. Main without a router
  has no long-running hydra process, so it isn't sampled.
- **Never in the way.** A line is one `write(2)` with `O_APPEND`, so the
  machine's many hydra processes don't interleave; anything that fails is
  skipped. `hydra log` and `hydra report` don't write (reading isn't news),
  and `HYDRA_JOURNAL=off` turns a process off.
- **`hydra report`** is for asking for help: one Markdown file with
  versions, `hydra status --all` and `hydra slots` now, usage and command
  timings summarised from the journal, what went wrong grouped with counts,
  and the journal's lines (without the usage samples it summarised).

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
  move on, memory fills up. `hydra down --worktrees` and the memory the
  dashboard shows are what there is; a watcher that stops a stack nobody has
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
