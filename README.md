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
hydra              # main checkout: everything; worktree: only what it changed
hydra plan         # what runs here, on which ports, wired to what, and why
hydra up web       # also run web in this worktree
hydra down         # stops this checkout's instance only
```

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
out. Pull there when a worktree needs newer baseline code.

### Starting

`hydra` in a worktree:

1. Lists the changed files and picks the services to run here.
2. Takes a **slot** for the worktree (kept across runs). Slot N listens on
   each service's port + N × `slot_stride`.
3. Writes `.process-compose/hydra.yaml`: those services only, on the slot's
   ports, with each upstream env var pointing at the local copy if the
   service runs here and at the baseline otherwise.
4. Starts process-compose on that file, on the worktree's own socket, so
   `hydra down` or `hydra attach` there never touch the baseline.

In the main checkout `hydra` runs the whole file, like `process-compose`.

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
worker, and switches it per request on routed services. `temporal` is the
only kind so far.

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

## Open questions

- Database: most backend branches add migrations, and the database is shared.
  Planned: a worktree that changes migrations gets its own Supabase
  (`supabase start --stack`), filled from the baseline's database.
- Scheduled jobs: a worktree's worker sets up its own schedules.
- Deriving `paths` from the nx / Go dependency graph instead of by hand.
