# Hydra

Run many branches of a local stack in parallel without running many stacks.

One **baseline** stack (the main checkout) stays up under
[process-compose](https://github.com/F1bonacc1/process-compose). Each worktree
starts only the services it changed (frontend, backend, database, workers) and
points everything else at the baseline. A frontend-only branch reuses the
baseline API and database, and a schema change gets its own database.

Status: exploration. Planning lives in the TickTick list "Hydra".

## Open questions

- Change detection: changed paths → services, including shared packages.
- Database: second Supabase vs per-worktree Postgres database (template copy).
- Workers and queues: keep the baseline worker from running a worktree's jobs.
- Wiring: ports, URLs and env per worktree.
- Baseline lifecycle and cleanup.
