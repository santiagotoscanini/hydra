#!/usr/bin/env bash
# Demo: one frontend serving every worktree.
#
#  main      runs api, web, admin and hydra's router.
#  backend   changed the api: it runs only an api. Its web is main's copy,
#            reached through the worktree's own port, calling the worktree's api.
#  docs      changed nothing that runs: its web is main's copy and main's api.
#
#  Then an edit to the web in `backend` starts a web there (the router gives
#  the port up), and reverting it hands the port back.
#
# Everything happens in a throwaway repo under $TMPDIR.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
pc_port=${PC_PORT:-8199} # main's process-compose server; 8099 may be yours

repo="$(mktemp -d "${TMPDIR:-/tmp}/hydra-routed.XXXXXX")"
main="$repo/main"
# Built into the throwaway directory, not the checkout's bin/: with
# `make link`, that one is the hydra on your PATH.
(cd "$root" && go build -o "$repo/bin/" ./cmd/...)
H="$repo/bin/hydra"
cleanup() {
  for wt in backend docs; do (cd "$repo/$wt" 2>/dev/null && "$H" down >/dev/null 2>&1) || true; done
  (cd "$main" && "$H" down -p "$pc_port" >/dev/null 2>&1) || true
  rm -rf "$repo"
}
trap cleanup EXIT

g() { git -c user.name=demo -c user.email=demo@example.com "$@"; }
commit() { mkdir -p "$(dirname "$1")"; echo "$1" >"$1"; g add -A; g commit -qm "$1"; }

mkdir -p "$main" && cd "$main"
g init -q -b main
sed "s#command: bin/mocksvc#command: $repo/bin/mocksvc#" "$here/routed.yaml" >process-compose.yaml
cp "$here/web.cjs" web.cjs
printf '.process-compose/\n' >.gitignore
commit README
g worktree add -q -b backend "$repo/backend" main && (cd "$repo/backend" && mkdir -p apps/api && echo edit >apps/api/list.go)
g worktree add -q -b docs "$repo/docs" main && (cd "$repo/docs" && echo edit >NOTES.md)

chain() { curl -s -m 5 "localhost:$1" | python3 "$here/chain.py"; }
wait_for() { # port
  for _ in $(seq 100); do curl -sf -m 2 "localhost:$1" >/dev/null 2>&1 && return 0; sleep 0.1; done
  echo "nothing answered on :$1" >&2; return 1
}
wait_chain() { # port pattern
  for _ in $(seq 100); do chain "$1" 2>/dev/null | grep -q "$2" && return 0; sleep 0.2; done
  echo "never saw '$2' on :$1 (last: $(chain "$1" 2>&1))" >&2; return 1
}

echo "--- the main checkout's stack"
"$H" up -D -p "$pc_port" -L "$repo/main.log" >/dev/null
wait_for 21000

echo "--- worktree backend: changed the api"
cd "$repo/backend"
"$H" plan | sed "s#$repo#<repo>#g"
"$H" up -D -L "$repo/backend.log" 2>&1 >/dev/null | sed "s#$repo#<repo>#g"
b=$("$H" slots | awk '$1=="*"{print $2}')

echo
echo "--- worktree docs: changed nothing that runs"
cd "$repo/docs"
"$H" up -D -L "$repo/docs.log" 2>&1 >/dev/null | sed "s#$repo#<repo>#g"
d=$("$H" slots | awk '$1=="*"{print $2}')

web_b=$((21000 + b * 100)); web_d=$((21000 + d * 100)); admin_b=$((21001 + b * 100))
wait_chain "$web_b" "api(slot $b"
wait_for "$web_d"

echo
echo "main    web   :21000    $(chain 21000)"
echo "backend web   :$web_b    $(chain "$web_b")"
echo "backend admin :$admin_b    $(chain "$admin_b")"
echo "docs    web   :$web_d    $(chain "$web_d")"

echo
echo "--- backend edits the web: it gets its own copy on the same port"
(cd "$repo/backend" && mkdir -p apps/web && echo edit >apps/web/page.tsx)
wait_chain "$web_b" "web(slot $b"
echo "backend web   :$web_b    $(chain "$web_b")"

echo
echo "--- backend reverts that edit: main's copy serves it again"
rm "$repo/backend/apps/web/page.tsx"
wait_chain "$web_b" "web(slot 0"
echo "backend web   :$web_b    $(chain "$web_b")"

echo
echo "--- backend goes down: its port closes"
(cd "$repo/backend" && "$H" down >/dev/null 2>&1)
for _ in $(seq 50); do curl -s -m 1 "localhost:$web_b" >/dev/null 2>&1 || break; sleep 0.2; done
curl -s -m 1 "localhost:$web_b" >/dev/null 2>&1 && echo "still answering on :$web_b" || echo "nothing listens on :$web_b"

echo
echo "--- what the router logged"
(cd "$main" && "$H" process logs hydra-router -p "$pc_port" -n 60 2>/dev/null)
