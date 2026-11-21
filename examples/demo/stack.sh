#!/usr/bin/env bash
# Demo: a stack of worktrees, main <- b1 (db) <- b2 (api) <- b3 (web-b),
# run from b3 with the live watcher.
#
#  1. `hydra up` in b3 runs db from b1, api from b2 and web-b from b3.
#     b2's api edit is uncommitted, like an agent's work in progress.
#  2. An edit to the worker in b2's worktree starts the worker (from b2).
#  3. Reverting that edit stops it again.
#  4. Reverting b2's api edit stops the api; web-b is pointed back at the
#     baseline's api. 5. Redoing the edit brings it back and rewires web-b.
#
# Everything happens in a throwaway repo under $TMPDIR.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
(cd "$root" && go build -o bin/ ./cmd/...)
H="$root/bin/hydra"
pc_port=${PC_PORT:-8199} # the baseline's process-compose server; 8099 may be yours

repo="$(mktemp -d "${TMPDIR:-/tmp}/hydra-stack.XXXXXX")"
main="$repo/main"
cleanup() {
  (cd "$repo/b3" 2>/dev/null && "$H" down >/dev/null 2>&1) || true
  (cd "$main" && "$H" down -p "$pc_port" >/dev/null 2>&1) || true
  rm -rf "$repo"
}
trap cleanup EXIT

g() { git -c user.name=demo -c user.email=demo@example.com "$@"; }
commit() { mkdir -p "$(dirname "$1")"; echo "$1" >"$1"; g add -A; g commit -qm "$1"; }

mkdir -p "$main" && cd "$main"
g init -q -b main
sed "s#command: bin/mocksvc.*#command: $root/bin/mocksvc#" "$here/process-compose.yaml" >process-compose.yaml
printf '.process-compose/\n' >.gitignore
commit README
g worktree add -q -b b1 "$repo/b1" main && (cd "$repo/b1" && commit db/002_add_column.sql)
g worktree add -q -b b2 "$repo/b2" b1 && (cd "$repo/b2" && mkdir -p apps/api && echo edit >apps/api/users.go)
g worktree add -q -b b3 "$repo/b3" b2 && (cd "$repo/b3" && mkdir -p apps/web-b && echo edit >apps/web-b/page.tsx)

echo "--- baseline in main"
"$H" up -D -p "$pc_port" -L "$repo/main.log" >/dev/null

echo "--- stack, run from b3"
cd "$repo/b3"
"$H" plan | sed "s#$repo#<repo>#g"
"$H" up -D -L "$repo/b3.log" >/dev/null

wait_for() { # port
  for _ in $(seq 100); do curl -sf "localhost:$1" >/dev/null 2>&1 && return 0; sleep 0.1; done
  echo "nothing answered on :$1" >&2; return 1
}
running() { "$H" process list 2>/dev/null | grep -vE '^(containers|hydra-watch)$' | sort | tr '\n' ' '; }
slot=$("$H" slots | awk '$1=="*"{print $2}')
web_b=$((20001 + slot * 100))
wait_for "$web_b"
echo
echo "web-b (slot $slot): $(curl -s "localhost:$web_b" | python3 "$here/chain.py")"
echo "running in b3's project: $(running)"

echo
echo "--- edit the worker in b2's worktree"
(cd "$repo/b2" && mkdir -p apps/worker && echo edit >apps/worker/job.go)
for _ in $(seq 50); do running | grep -qw worker && break; sleep 0.2; done
echo "running in b3's project: $(running)"

echo
echo "--- revert that edit"
rm "$repo/b2/apps/worker/job.go"
for _ in $(seq 50); do running | grep -qw worker || break; sleep 0.2; done
echo "running in b3's project: $(running)"

chain() { curl -s "localhost:$web_b" | python3 "$here/chain.py"; }
api_slot() { chain | grep -oE 'api\(slot [0-9]+' | grep -oE '[0-9]+$'; }

echo
echo "--- revert b2's api edit"
rm "$repo/b2/apps/api/users.go"
for _ in $(seq 50); do [ "$(api_slot)" = 0 ] && break; sleep 0.2; done
echo "running in b3's project: $(running)"
echo "web-b: $(chain)"

echo
echo "--- redo it"
echo edit >"$repo/b2/apps/api/users.go"
for _ in $(seq 100); do [ "$(api_slot)" = "$slot" ] && break; sleep 0.2; done
echo "running in b3's project: $(running)"
echo "web-b: $(chain)"

echo
echo "--- what the watcher logged"
"$H" process logs hydra-watch -n 60 2>/dev/null
