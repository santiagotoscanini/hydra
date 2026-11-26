#!/usr/bin/env bash
# Demo: the dashboard (bare `hydra` in a terminal), driven in tmux.
#
#  1. It lists main and every worktree by branch as a tree, each branch
#     under the one it is stacked on: whether its stack is up, how it
#     stands against its parent (behind, a conflict, in step but on a
#     branch that is behind), with the checkout it was started in chosen,
#     and the chosen one's services and problems in a panel beside or below.
#     A PR stack whose lower branches have no worktree shows them in their
#     place, hollow and muted, with only m to offer. Each row has its PR
#     (from a fake gh: the api's open, deeper's merged), and the header
#     counts the worktrees on a merged PR.
#  2. u starts a worktree's stack, ⏎ opens it (process-compose attach), and
#     F10 or ctrl-c, then Yes, comes back to the dashboard with the stack
#     still running. d stops it, after asking.
#  3. q leaves the dashboard; main's stack keeps running.
#
# The chosen row's background follows tmux's window-style, light or dark,
# as it changes.
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
  for wt in api web clash stacked deeper ring; do (cd "$repo/$wt" 2>/dev/null && "$H" down >/dev/null 2>&1) || true; done
  for wt in api web clash stacked deeper ring; do (cd "$main" 2>/dev/null && "$H" slots free "$repo/$wt" >/dev/null 2>&1) || true; done
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

# A repository with five worktrees: one that changed the api, one the web
# app, one whose change conflicts with main's, one stacked on the web one,
# and one stacked on that. main moves on after the web one is cut, so the
# stack is behind from web down.
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
# ring-schema <- ring-core <- ring-web, only ring-web checked out.
g worktree add -q -b ring-schema "$repo/ringtmp" main
(cd "$repo/ringtmp" && put db/ring.sql && g add -A && g commit -qm "ring: schema" && g checkout -q -b ring-core && put apps/api/ring.go && g add -A && g commit -qm "ring: core")
g worktree remove "$repo/ringtmp"
g worktree add -q -b ring-web "$repo/ring" ring-core
(cd "$repo/ring" && put apps/web/ring.tsx && g add -A && g commit -qm "ring: web")
put apps/api/users.go "main's"; g add -A; g commit -qm "api: users on main"
g -C "$repo/ring" merge -q --no-edit main # straight into the top: its stack stays behind
g worktree add -q -b stacked "$repo/stacked" web
(cd "$repo/stacked" && put apps/web/more.tsx && g add -A && g commit -qm "web: more")
g worktree add -q -b deeper "$repo/deeper" stacked
(cd "$repo/deeper" && put apps/web/deeper.tsx && g add -A && g commit -qm "web: deeper")
"$H" up -D -L "$repo/main.log" >/dev/null

# A fake gh (HYDRA_GH), so nothing reaches GitHub: the api's PR is open with
# its checks passing, deeper's is merged. hydra asks only with a remote.
g remote add origin https://example.invalid/demo/repo.git
cat >"$repo/bin/gh" <<'SH'
#!/bin/sh
cat <<'JSON'
[{"number":7,"state":"OPEN","isDraft":false,"headRefName":"api","baseRefName":"main","reviewDecision":"REVIEW_REQUIRED","url":"https://example.invalid/demo/repo/pull/7","statusCheckRollup":[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"}]},
 {"number":8,"state":"MERGED","isDraft":false,"headRefName":"deeper","baseRefName":"stacked","reviewDecision":"APPROVED","url":"https://example.invalid/demo/repo/pull/8","statusCheckRollup":[]}]
JSON
SH
chmod +x "$repo/bin/gh"

T new -d -s keep "sleep 100000"
start() { # cols rows
  T kill-session -t d 2>/dev/null || true
  T new -d -s d -x "$1" -y "$2" -c "$repo/api" \
    "env PC_PORT_NUM=$PC_PORT_NUM HYDRA_CACHE_DIR=$repo/cache HYDRA_GH=$repo/bin/gh $H; echo EXITED-\$?; sleep 600"
  waitfor 'Checkouts [0-9]'
  sleep 1
}

echo "--- 1. bare hydra in the api worktree"
start 80 34
waitfor '#7 ✓'
screen "80 columns"
T capture-pane -p -t d | grep -qE '▌├─○ api +down +#7 ✓ +1 behind main' || fail "the api worktree isn't chosen, with its open PR"
T capture-pane -p -t d | grep -qE '● main +up +up to date' || fail "main isn't up"
T capture-pane -p -t d | grep -qE '├─○ clash +down +1 behind main · 1 conflict' || fail "the conflict isn't shown"
T capture-pane -p -t d | grep -qE '└─○ web +down +1 behind main' || fail "web, last on main, isn't drawn so"
T capture-pane -p -t d | grep -qE '│ {3}└─○ stacked +down +↑ web is behind' || fail "the stack isn't under web, out of date through it"
T capture-pane -p -t d | grep -qE '│ {5}└─○ deeper +down +#8 merged +PR merged: nothing to do' || fail "the second level isn't under stacked, marked as on a merged PR"
T capture-pane -p -t d | grep -qE '├─◇ ring-schema +no checkout +1 behind main' || fail "the stack's bottom branch, with no checkout, isn't shown behind main"
T capture-pane -p -t d | grep -qE '│ └─◇ ring-core +no checkout +↑ ring-schema is behind' || fail "the middle branch, with no checkout, isn't under ring-schema"
T capture-pane -p -t d | grep -qE '│ {3}└─○ ring-web +down +↑ ring-schema is behind' || fail "ring-web merged main, but its stack didn't: it isn't out of date"
T capture-pane -p -t d | grep -qE '0 of 6 worktrees up · 8 out of date · 1 on a merged PR' || fail "the header doesn't count what is out of date, and on a merged PR"
T capture-pane -p -t d | grep -qE '◎ PR  #7 open · review required · checks passing' || fail "the chosen worktree's PR isn't first in its detail"
T capture-pane -p -t d | grep -qE '◆ Runs here, and why' || fail "the chosen worktree's services aren't shown"
T capture-pane -p -t d | python3 -c 'import sys; sys.exit(any(len(l.rstrip("\n")) > 80 for l in sys.stdin))' || fail "a line is wider than 80 columns"
T send-keys -t d '?'
waitfor 'PR status from gh, read just now'
T send-keys -t d Escape
waitfor 'Checkouts [0-9]'
T send-keys -t d j j
waitfor '▌├─◇ ring-schema'
screen "a branch with no checkout, chosen"
T capture-pane -p -t d | grep -qE 'ring-schema · no checkout' || fail "the detail doesn't say it has no checkout"
T capture-pane -p -t d | grep -qE 'nothing: no worktree has it checked out' || fail "the detail doesn't say nothing runs it"
T capture-pane -p -t d | grep -qE '^ m merge main +U update all' || fail "the footer doesn't offer m alone"
T send-keys -t d u
waitfor 'ring-schema has no checkout: nothing runs it'
start 150 24
screen "150 columns"
T capture-pane -p -t d | grep -qE '╭─ Checkouts.*╭─ api' || fail "a wide terminal shows the detail beside the list"
T capture-pane -p -t d | grep -qE '╭─ Problems' || fail "a wide terminal shows the problems under the list"
# The chosen row's background follows the terminal's theme, also when it
# changes while the dashboard runs. A tmux with no client attached and no
# window-style of its own doesn't know its background, so the row has none;
# with a style it answers, and tells the dashboard when the style changes.
bgrow() { T capture-pane -e -p -t d | grep -F '▌' | grep -qF $'\e[48;2;'"$1"'m'; }
T capture-pane -e -p -t d | grep -F '▌' | grep -qF $'\e[48;2;' && fail "an unknown theme still gave the chosen row a background"
T set -t d window-style 'bg=#2e3440'
for _ in $(seq 20); do bgrow '38;75;120' && break; sleep 0.25; done
bgrow '38;75;120' || fail "a dark terminal's chosen row isn't dark blue"
T set -t d window-style 'bg=#ffffff'
for _ in $(seq 20); do bgrow '191;219;254' && break; sleep 0.25; done
bgrow '191;219;254' || fail "a light terminal's chosen row isn't light blue"
T set -u -t d window-style

echo "--- 2. u, ⏎, F10 and Yes, d"
T send-keys -t d u
waitfor 'starting'
screen "u: the row and the panel say it is starting"
waitfor '✓ hydra up -D in api'
waitfor '● api +up'
screen "u started the api worktree's stack"
T resize-window -t d -x 100 -y 32
sleep 1
screen "100 columns"
T capture-pane -p -t d | python3 -c 'import sys; sys.exit(any(len(l.rstrip("\n")) > 100 for l in sys.stdin))' || fail "a line is wider than 100 columns"
T resize-window -t d -x 150 -y 24
sleep 0.5
[[ "$(procs "$repo/api")" == *api* ]] || fail "the stack isn't up"
T send-keys -t d Enter
waitfor 'Process Compose'
T send-keys -t d F10
waitfor 'Detach from the remote'
screen "⏎ opened it; F10 asks"
T send-keys -t d Enter
waitfor 'Checkouts'
[[ "$(procs "$repo/api")" == *api* ]] || fail "leaving attach stopped the stack"
echo "back in the dashboard; the stack still runs: $(procs "$repo/api")"
T send-keys -t d Enter
waitfor 'Process Compose'
T send-keys -t d C-c
waitfor 'Detach from the remote'
T send-keys -t d Enter
waitfor 'Checkouts'
[[ "$(procs "$repo/api")" == *api* ]] || fail "ctrl-c in attach stopped the stack"
echo "the same after ctrl-c: $(procs "$repo/api")"
T send-keys -t d d
waitfor "Stop api's stack"
T send-keys -t d y
waitfor '○ api +down'
[ -z "$(procs "$repo/api")" ] || fail "d left the stack running"
echo "d, y: stopped"

echo "--- 3. q"
T send-keys -t d q
waitfor 'EXITED-0'
[[ "$(procs "$main")" == *web* ]] || fail "quitting stopped main"
echo "the dashboard is gone; main still runs: $(procs "$main")"
echo
echo "all checks passed"
