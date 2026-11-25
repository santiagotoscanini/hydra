#!/usr/bin/env bash
# Demo: the dashboard (bare `hydra` in a terminal), driven in tmux.
#
#  1. It lists main and every worktree: what each runs, its slot, whether
#     its stack is up, how it stands against main (behind, a conflict,
#     stacked on another branch), with the checkout it was started in
#     chosen.
#  2. u starts a worktree's stack, ⏎ opens it (process-compose attach), and
#     F10 or ctrl-c, then Yes, comes back to the dashboard with the stack
#     still running. d stops it, after asking.
#  3. q leaves the dashboard; main's stack keeps running.
#
# Needs tmux and node. Everything happens in a throwaway repo under $TMPDIR;
# each step checks the screen, so this doubles as a test.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
command -v tmux >/dev/null || { echo "skipped: tmux isn't installed"; exit 0; }
command -v node >/dev/null || { echo "skipped: node isn't installed"; exit 0; }
export PC_PORT_NUM=${PC_PORT:-8197} # main's process-compose server; 8099 may be yours
unset PC_ADDRESS

repo="$(mktemp -d "${TMPDIR:-/tmp}/hydra-dashboard.XXXXXX")"
repo="$(cd "$repo" && pwd -P)"
main="$repo/main"
sock="$repo/tmux.sock"
T() { tmux -S "$sock" "$@"; }
(cd "$root" && go build -o "$repo/bin/" ./cmd/...)
H="$repo/bin/hydra"
cleanup() {
  cd /
  T kill-server 2>/dev/null || true
  for wt in api web clash stacked; do (cd "$repo/$wt" 2>/dev/null && "$H" down >/dev/null 2>&1) || true; done
  for wt in api web clash stacked; do (cd "$main" 2>/dev/null && "$H" slots free "$repo/$wt" >/dev/null 2>&1) || true; done
  (cd "$main" 2>/dev/null && "$H" down >/dev/null 2>&1) || true
  rm -rf "$repo"
}
trap cleanup EXIT

g() { git -c user.name=demo -c user.email=demo@example.com -c commit.gpgsign=false "$@"; }
put() { mkdir -p "$(dirname "$1")"; echo "${2:-$1}" >"$1"; }
fail() { echo "FAILED: $*" >&2; T capture-pane -p -t d >&2 || true; exit 1; }
screen() { echo "--- $1"; T capture-pane -p -t d | sed -e "s#$repo#<repo>#g" -e 's/[[:space:]]*$//' | cat -s; echo; }
waitfor() { for _ in $(seq 80); do T capture-pane -p -t d | grep -qE -- "$1" && return 0; sleep 0.25; done; fail "no /$1/ on the screen"; }
procs() { (cd "$1" && "$H" process list 2>/dev/null | tr '\n' ' '); }

# A repository with four worktrees: one that changed the api, one the web
# app, one whose change conflicts with main's, and one stacked on the web one.
g init -q -b main "$main"
cd "$main"
cat >process-compose.yaml <<YAML
version: "0.5"
x-hydra:
  slot_stride: 100
  max_slots: 8
  host: 127.0.0.1
  ignore: [".gitignore", "process-compose.yaml"]
processes:
  db:
    command: $repo/bin/mocksvc
    x-hydra: { shared: true, port: 23330, url: "postgres://{host}:{port}/app" }
  api:
    command: $repo/bin/mocksvc
    depends_on: { db: { condition: process_started } }
    x-hydra: { port: 23310, paths: ["apps/api/**"], upstreams: { DATABASE_URL: db } }
  web:
    command: $repo/bin/mocksvc
    depends_on: { api: { condition: process_started } }
    x-hydra: { port: 23300, paths: ["apps/web/**"], upstreams: { API_URL: api } }
YAML
printf '.process-compose/\n' >.gitignore
put apps/api/users.go; put apps/web/page.tsx
g add -A; g commit -qm init
g worktree add -q -b api "$repo/api" main
(cd "$repo/api" && put apps/api/orders.go && g add -A && g commit -qm "api: orders")
g worktree add -q -b web "$repo/web" main
(cd "$repo/web" && put apps/web/cart.tsx && g add -A && g commit -qm "web: cart")
g worktree add -q -b clash "$repo/clash" main
(cd "$repo/clash" && put apps/api/users.go "the branch's" && g add -A && g commit -qm "api: users")
put apps/api/users.go "main's"; g add -A; g commit -qm "api: users on main"
g worktree add -q -b stacked "$repo/stacked" web
(cd "$repo/stacked" && put apps/web/more.tsx && g add -A && g commit -qm "web: more")
"$H" up -D -L "$repo/main.log" >/dev/null

T new -d -s keep "sleep 100000"
start() { # cols rows
  T kill-session -t d 2>/dev/null || true
  T new -d -s d -x "$1" -y "$2" -c "$repo/api" \
    "env PC_PORT_NUM=$PC_PORT_NUM HYDRA_CACHE_DIR=$repo/cache $H; echo EXITED-\$?; sleep 600"
  waitfor 'level with main|behind main'
  sleep 1
}

echo "--- 1. bare hydra in the api worktree"
start 80 24
screen "80 columns"
T capture-pane -p -t d | grep -qE '▸\* api +- +down +api' || fail "the api worktree isn't chosen"
T capture-pane -p -t d | grep -qE 'main +0 +up +3/3 ok' || fail "main isn't up"
T capture-pane -p -t d | grep -qE 'clash .*conflicts\(1\)' || fail "the conflict isn't shown"
T capture-pane -p -t d | grep -qE 'stacked .*on web' || fail "the stack isn't shown"
T capture-pane -p -t d | python3 -c 'import sys; sys.exit(any(len(l.rstrip("\n")) > 80 for l in sys.stdin))' || fail "a line is wider than 80 columns"
start 150 24
screen "150 columns"
T capture-pane -p -t d | grep -qE 'BRANCH' || fail "a wide terminal shows the branch"

echo "--- 2. u, ⏎, F10 and Yes, d"
T send-keys -t d u
waitfor '✓ hydra up -D in api'
waitfor 'api +[0-9]+ +up'
screen "u started the api worktree's stack"
[[ "$(procs "$repo/api")" == *api* ]] || fail "the stack isn't up"
T send-keys -t d Enter
waitfor 'Process Compose'
T send-keys -t d F10
waitfor 'Detach from the remote'
screen "⏎ opened it; F10 asks"
T send-keys -t d Enter
waitfor 'CHECKOUT'
[[ "$(procs "$repo/api")" == *api* ]] || fail "leaving attach stopped the stack"
echo "back in the dashboard; the stack still runs: $(procs "$repo/api")"
T send-keys -t d Enter
waitfor 'Process Compose'
T send-keys -t d C-c
waitfor 'Detach from the remote'
T send-keys -t d Enter
waitfor 'CHECKOUT'
[[ "$(procs "$repo/api")" == *api* ]] || fail "ctrl-c in attach stopped the stack"
echo "the same after ctrl-c: $(procs "$repo/api")"
T send-keys -t d d
waitfor "Stop api's stack"
T send-keys -t d y
waitfor 'api +[0-9]+ +down'
[ -z "$(procs "$repo/api")" ] || fail "d left the stack running"
echo "d, y: stopped"

echo "--- 3. q"
T send-keys -t d q
waitfor 'EXITED-0'
[[ "$(procs "$main")" == *web* ]] || fail "quitting stopped main"
echo "the dashboard is gone; main still runs: $(procs "$main")"
echo
echo "all checks passed"
