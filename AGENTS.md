# Hydra

A process-compose wrapper: the main checkout runs the whole stack once, and
each git worktree runs only the services its branch changed, wired to the
main checkout's copies of the rest. See `README.md` for what it does and
`docs/design.md` for the rules in full.

## Layout

| Path | What |
|---|---|
| `cmd/hydra/` | The CLI: the process-compose wrapper (`wrap.go`), `plan`/`render`/`slots`, the checks behind `hydra status` and `hydra up` (`check.go`, `status.go`, and `ports.go`, which names the program holding a port), the status page (`ui.go`, and `ui.html`, which is embedded in the binary), `hydra update` (`update.go`), cleaning up after removed worktrees (`reap.go`), the certificate behind TLS fronts and `hydra trust` (`tls.go`), `hydra version` (`version.go`), and the processes hydra adds (`watch.go`, `router.go`, `prepare.go`, `supabase.go`) |
| `internal/` | `config` (the `x-hydra` keys), `plan` (what runs where, and what is out of step with main), `render` (the generated file), `stack` (stacked worktrees), `router` + `routes` + `hook` (routed services), `pc` (process-compose's HTTP API), `slots`, `gitx`, `glob`, `changes` |
| `examples/demo/` | Mock stacks and scripts that run hydra end to end (`run.sh`, `stack.sh`, `routed.sh`, `update.sh`, `tls.sh`) |
| `site/` | The site at <https://hydra.toscanini.me>: static HTML, no build step, deployed by `.github/workflows/pages.yml` on push to `main` |

## Keep the docs in sync with the code

`site/docs/index.html` is the manual, and the only thing its owner reads:
someone who never opens the code has to be able to set hydra up, use it
every day and fix what goes wrong from that page alone. Every change to
behaviour, a config key, a command, a flag, an env var, a file hydra
writes, a message of `hydra status` or the output of `hydra plan` updates,
in the same commit, every place that describes it:

- `site/docs/index.html`: the "Using it" sections (what to type, and the
  output shown, which is copied from real runs), "Every message, and what
  to do" (the texts in `cmd/hydra/check.go`, `status.go`, `supabase.go` and
  `ports.go`), the option tables, the command table, "Files hydra writes",
  the sidebar
- `README.md` (the short version)
- `docs/design.md` (the rules and why)
- `site/index.html` and `site/app.js` (the homepage, its animation and the
  planner demo, which reimplements `hydra plan` for a sample config)
- `hydra --help` (`usage` in `cmd/hydra/main.go`)

Removed things are removed from all of them too; renamed things are renamed
everywhere. Before committing, search the repo for the old name or
behaviour (`rg -n '<key>' README.md docs site cmd`) and fix what is stale.

When `hydra plan` changes, check that the site's planner still matches it:
render each combination of the demo's file chips with both and compare. The
sample config it mirrors is the quick start in `site/docs/index.html`.

## Verifying

- `go vet ./... && go test ./...` (the hook tests need `node`).
- The demos: `./examples/demo/run.sh`, `./examples/demo/stack.sh`,
  `./examples/demo/routed.sh`, `./examples/demo/update.sh`,
  `./examples/demo/tls.sh`. Each builds hydra into a throwaway directory of
  its own (never `bin/`, which may be the `hydra` on PATH) and cleans up
  after itself. `tls.sh` uses a CA of its own (`HYDRA_TLS_DIR`), never the
  user's. `update.sh` checks what every step printed and exits non-zero
  when one is off: it is the end-to-end test of `hydra status`, the checks
  before `hydra up`, `hydra update`, a second start, the order in which an
  update starts things, a removed worktree and a killed process-compose.
- CI (`.github/workflows/test.yml`) runs the tests and all five demos on
  every push to `main`.
- What needs Docker isn't covered by either: a worktree's own Supabase
  (`cmd/hydra/supabase.go`, `reap.go`). After a change there, run a real
  stack in a throwaway project under the home directory (Colima only
  mounts that) on ports below 49152, rename its branch, and remove its
  worktree; `supabase stack list --output-format json` shows what is left.
- The site: open `site/index.html` in a browser, in light and dark, and at
  phone width; there must be no horizontal scroll.
- The status page (`cmd/hydra/ui.html`): the same, with data. `update.sh`
  reads its JSON; for the look, run `hydra ui` in a repository with a few
  worktrees, some running and one behind main.

## Before touching a running stack

`make build` replaces the `hydra` on PATH when the checkout is linked
(`make link`), so a stack someone is using gets that build the next time
`hydra` runs there. A change to what the main checkout's file renders to
(slot 0) then reaches the running stack as a reload:

- Run `hydra update --dry-run` there first. Its reload line says which
  processes change, are added, dropped or switched on. "SWITCHES ON" means
  the render lost a `disabled: true`: stop.
- Never test against a stack someone is using. The demos and a throwaway
  repository cover everything; `update.sh` shows how to build one.

