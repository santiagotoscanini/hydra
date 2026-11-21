# Hydra

Run many branches of a local stack in parallel without running many stacks.

One **baseline** stack (the main checkout) stays up under
[process-compose](https://github.com/F1bonacc1/process-compose). Each worktree
starts only the services it changed (frontend, backend, database, workers) and
points everything else at the baseline. A frontend-only branch reuses the
baseline API and database, and a schema change gets its own database.

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

While it runs, a `hydra-watch` process starts a service as soon as a worktree
changes it and stops it (pointing back at the baseline) when the change is
reverted. Stacked worktrees (`main ← b1 ← b2 ← b3`) run each service from the
worktree that changed it, and are all watched.

See [docs/design.md](docs/design.md) for the fields and rules.

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
- Scheduled jobs: a worktree's worker sets up its own schedules.
- Deriving `paths` from the nx / Go dependency graph instead of by hand.
