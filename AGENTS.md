# Hydra

A process-compose wrapper: the main checkout runs the whole stack once, and
each git worktree runs only the services its branch changed, wired to the
main checkout's copies of the rest. See `README.md` for what it does and
`docs/design.md` for the rules in full.

## Layout

| Path | What |
|---|---|
| `cmd/hydra/` | The CLI: the process-compose wrapper (`wrap.go`), `plan`/`render`/`slots`, the checks behind `hydra status` and `hydra up` (`check.go`, `status.go`), the status page (`ui.go`, and `ui.html`, which is embedded in the binary), `hydra update` (`update.go`), the certificate behind TLS fronts and `hydra trust` (`tls.go`), and the processes hydra adds (`watch.go`, `router.go`, `prepare.go`) |
| `internal/` | `config` (the `x-hydra` keys), `plan` (what runs where, and what is out of step with main), `render` (the generated file), `stack` (stacked worktrees), `router` + `routes` + `hook` (routed services), `pc` (process-compose's HTTP API), `slots`, `gitx`, `glob`, `changes` |
| `examples/demo/` | Mock stacks and scripts that run hydra end to end (`run.sh`, `stack.sh`, `routed.sh`, `update.sh`, `tls.sh`) |
| `site/` | The site at <https://hydra.toscanini.me>: static HTML, no build step, deployed by `.github/workflows/pages.yml` on push to `main` |

## Keep the docs in sync with the code

Every change to behaviour, a config key, a command, a flag, an env var or
the output of `hydra plan` updates, in the same commit, every place that
describes it:

- `README.md`
- `docs/design.md`
- `site/docs/index.html` (option tables, sections, the sidebar)
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
  `./examples/demo/tls.sh`. Each builds hydra and cleans up after itself.
  `tls.sh` uses a CA of its own (`HYDRA_TLS_DIR`), never the user's. `update.sh` checks what every step printed and
  exits non-zero when one is off: it is the end-to-end test of `hydra
  status`, the checks before `hydra up`, and `hydra update`.
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

