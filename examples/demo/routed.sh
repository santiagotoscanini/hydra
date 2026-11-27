#!/usr/bin/env bash
# Demo: one frontend serving every worktree.
#
#  main      runs api, web, admin and hydra's router.
#  backend   changed the api: it runs only an api. Its web is main's copy,
#            reached through the worktree's own port, calling the worktree's api.
#  docs      changed nothing that runs: its web is main's copy and main's api.
#
#  Then an edit to the web in `backend` starts a web there (the router gives
#  the port up), and reverting it hands the port back. And an edit to the api
#  in `docs`, which started with nothing to run, starts an api there; the
#  edit after that restarts it.
#
#  On the way, each worktree's address by name: <worktree>.localhost:21900
#  is its web (its own copy, or main's through its port), localhost:21900
#  main's, and the same on :21910 for the api.
#
#  Then the tunnel (a stand-in for cloudflared on :21079): main's api answers
#  it until `backend` takes it, a 502 says so while backend's api is stopped,
#  and `hydra down` in backend gives it back to main.
#
#  Last, a worktree whose branch sets `idle_stop: 6s`: requests through its
#  port keep its stack up, and once they stop hydra stops it, says so in
#  `hydra status` and `hydra status --json`, and `hydra up` starts it again.
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
  for wt in backend docs idle; do (cd "$repo/$wt" 2>/dev/null && "$H" down >/dev/null 2>&1) || true; done
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
echo "--- the same, by name: <worktree>.localhost on the hosts ports"
fail() { echo "FAILED: $*" >&2; exit 1; }
byname() { # name port: what answers at http://NAME.localhost:PORT, resolved to 127.0.0.1 as a browser does
  curl -s -m 5 --resolve "$1.localhost:$2:127.0.0.1" "http://$1.localhost:$2/"
}
expect() { # name port pattern
  got="$(byname "$1" "$2" | python3 "$here/chain.py" 2>&1)" || true
  grep -q "$3" <<<"$got" || fail "$1.localhost:$2: want '$3', got: $got $(byname "$1" "$2")"
  printf '%-27s %s\n' "$1.localhost:$2" "$got"
}
expect main 21900 "^web(slot 0.*api(slot 0"
expect backend 21900 "^web(slot 0.*api(slot $b"
expect docs 21900 "^web(slot 0.*api(slot 0"
expect backend 21910 "^api(slot $b"
expect docs 21910 "^api(slot 0"
got="$(curl -s -m 5 localhost:21900 | python3 "$here/chain.py")"
grep -q "^web(slot 0.*api(slot 0" <<<"$got" || fail "localhost:21900: $got"
printf '%-27s %s\n' "localhost:21900" "$got"
out="$(cd "$repo/backend" && "$H" status 2>&1 || true)"
grep -E '^address' <<<"$out"
grep -q "^address   api http://backend.localhost:21910, web http://backend.localhost:21900$" <<<"$out" || fail "hydra status: $out"
addr="$(cd "$main" && "$H" status --json | python3 -c '
import json, sys
w = {w["branch"]: w for w in json.load(sys.stdin)["worktrees"]}["docs"]
print({s["name"]: s.get("address") for s in w["services"]}["web"])')"
echo "hydra status --json, docs web: $addr"
[ "$addr" = "http://docs.localhost:21900" ] || fail "status --json address: $addr"

echo
echo "--- backend edits the web: it gets its own copy on the same port"
(cd "$repo/backend" && mkdir -p apps/web && echo edit >apps/web/page.tsx)
wait_chain "$web_b" "web(slot $b"
echo "backend web   :$web_b    $(chain "$web_b")"
expect backend 21900 "^web(slot $b"

echo
echo "--- backend reverts that edit: main's copy serves it again"
rm "$repo/backend/apps/web/page.tsx"
wait_chain "$web_b" "web(slot 0"
echo "backend web   :$web_b    $(chain "$web_b")"

echo
echo "--- docs edits the api: one starts there, and the next edit restarts it"
api_d=$((21010 + d * 100))
api_pid() { (cd "$repo/docs" && "$H" process get api -o json 2>/dev/null) | python3 -c 'import json, sys
d = json.load(sys.stdin)
print((d[0] if isinstance(d, list) else d).get("pid", 0))' 2>/dev/null || echo 0; }
(cd "$repo/docs" && mkdir -p apps/api && echo one >apps/api/list.go)
wait_for "$api_d"
first=$(api_pid)
echo "docs    api   :$api_d    started by the first edit"
sleep 1 # let the first run settle before the edit that must restart it
(cd "$repo/docs" && echo two >apps/api/list.go)
for _ in $(seq 100); do now=$(api_pid); [ "$now" != "$first" ] && [ "$now" != 0 ] && break; sleep 0.2; done
if [ "$now" = "$first" ] || [ "$now" = 0 ]; then
  echo "the api in docs did not restart on an edit (pid $first)" >&2; exit 1
fi
wait_for "$api_d"
echo "docs    api   :$api_d    restarted by the second edit"

echo
echo "--- the tunnel: main's api, until a worktree takes it"
hook() { curl -s -m 5 -X POST "localhost:21079/hook" | python3 "$here/chain.py" 2>&1; }
wait_for 21079
got="$(hook)"; echo "tunnel :21079 -> $got"
grep -q "^api(slot 0" <<<"$got" || fail "the tunnel before a take: $got"
(cd "$main" && "$H" tunnel) | sed "s#$repo#<repo>#g"
(cd "$repo/backend" && "$H" tunnel take) | sed "s#$repo#<repo>#g"
for _ in $(seq 20); do hook | grep -q "^api(slot $b" && break; sleep 0.2; done
got="$(hook)"; echo "tunnel :21079 -> $got"
grep -q "^api(slot $b" <<<"$got" || fail "the tunnel after backend took it: $got"
out="$(cd "$repo/backend" && "$H" status 2>&1 || true)"
grep '^tunnel' <<<"$out"
grep -q "^tunnel    hooks (https://hooks.example.test) comes here: api :$((21010 + b * 100))$" <<<"$out" || fail "backend's status: $out"
out="$(cd "$repo/docs" && "$H" status 2>&1 || true)"
grep -A1 'webhooks through tunnel' <<<"$out"
grep -q "^! webhooks through tunnel hooks (https://hooks.example.test) go to backend's api, not this worktree's.$" <<<"$out" || fail "docs's status: $out"
json="$(cd "$main" && "$H" status --json | python3 -c '
import json, sys, os
s = json.load(sys.stdin)
t = s["tunnels"][0]
w = {w["branch"]: w for w in s["worktrees"]}["backend"]
print(t["name"], os.path.basename(t["holder"]), t["target"], w.get("tunnel"))')"
echo "hydra status --json: $json"
[ "$json" = "hooks backend backend:$((21010 + b * 100)) ['hooks']" ] || fail "status --json: $json"
(cd "$repo/backend" && "$H" process stop api >/dev/null 2>&1)
for _ in $(seq 20); do curl -s -m 2 -o /dev/null -w '%{http_code}' -X POST localhost:21079/hook | grep -q 502 && break; sleep 0.2; done
body="$(curl -s -m 5 -X POST localhost:21079/hook)"
echo "backend's api stopped: $body"
grep -q "tunnel hooks is held by worktree backend, and its api isn't answering" <<<"$body" || fail "the 502: $body"
out="$(cd "$repo/backend" && "$H" status 2>&1 || true)"
grep -q "^✗ this worktree holds tunnel hooks (https://hooks.example.test), and its api is" <<<"$out" || fail "backend's status with its api stopped: $out"
(cd "$repo/backend" && "$H" process start api >/dev/null 2>&1)
for _ in $(seq 50); do hook | grep -q "^api(slot $b" && break; sleep 0.2; done
grep -q '"ev":"tunnel".*"action":"take"' "$main"/.git/hydra-journal/*.jsonl || fail "no tunnel event in the journal"

echo
echo "--- backend goes down: its port closes, and the tunnel goes back to main"
(cd "$repo/backend" && "$H" down 2>&1 >/dev/null | grep tunnel) || fail "hydra down didn't say it gave the tunnel back"
for _ in $(seq 20); do hook | grep -q "^api(slot 0" && break; sleep 0.2; done
got="$(hook)"; echo "tunnel :21079 -> $got"
grep -q "^api(slot 0" <<<"$got" || fail "the tunnel after backend went down: $got"
for _ in $(seq 50); do curl -s -m 1 "localhost:$web_b" >/dev/null 2>&1 || break; sleep 0.2; done
curl -s -m 1 "localhost:$web_b" >/dev/null 2>&1 && echo "still answering on :$web_b" || echo "nothing listens on :$web_b"
for _ in $(seq 50); do byname backend 21900 | grep -q 'no running worktree is called "backend"' && break; sleep 0.2; done
echo "backend.localhost:21900     $(byname backend 21900)"
byname backend 21900 | grep -q 'no running worktree is called "backend"' || fail "a stopped worktree's address"

echo
echo "--- idle: a branch with idle_stop: 6s"
g worktree add -q -b idle "$repo/idle" main
cd "$repo/idle"
awk '{print} /^  max_slots: 5$/{print "  idle_stop: 6s"}' process-compose.yaml >pc.tmp && mv pc.tmp process-compose.yaml
grep -q 'idle_stop: 6s' process-compose.yaml || fail "the sed didn't add idle_stop"
g commit -qam "idle_stop: 6s" # committed: nothing uncommitted counts as use
"$H" up -D -L "$repo/idle.log" >/dev/null 2>&1
i=$("$H" slots | awk '$1=="*"{print $2}')
web_i=$((21000 + i * 100))
wait_for "$web_i"
# Twelve seconds of requests through its port, one a second: twice the idle
# time, and the stack stays up.
for _ in $(seq 12); do curl -s -m 2 "localhost:$web_i" >/dev/null; sleep 1; done
"$H" status >/dev/null 2>&1 || fail "the stack stopped while it was used: $("$H" status 2>&1)"
echo "used for 12s: still up"
# Then nothing: within 6s and the watcher's next look, it stops.
for _ in $(seq 60); do [ -f .process-compose/idle-stopped ] && break; sleep 0.5; done
set +e; out="$("$H" status 2>&1)"; set -e
echo "$out" | grep -E '^(worktree|✗)' | sed -E "s#[^ ]*/hydra-routed\.[A-Za-z0-9]+#<repo>#g"
grep -q 'stopped after 6s idle .*hydra up starts it again' <<<"$out" || fail "hydra status: $out"
json="$(cd "$main" && "$H" status --json | python3 -c '
import json, sys
w = {w["branch"]: w for w in json.load(sys.stdin)["worktrees"]}["idle"]
print("up:", w["up"], "| idleStopped after:", (w.get("idleStopped") or {}).get("after"))')"
echo "hydra status --json: $json"
[ "$json" = "up: False | idleStopped after: 6s" ] || fail "status --json: $json"
grep -q '"ev":"idle".*"after":"6s"' "$(git rev-parse --git-common-dir)"/hydra-journal/*.jsonl || fail "no idle event in the journal"
"$H" up -D -L "$repo/idle.log" >/dev/null 2>&1
wait_for "$web_i"
[ ! -f .process-compose/idle-stopped ] || fail "hydra up left the idle note"
echo "hydra up: running again"
"$H" down >/dev/null 2>&1

echo
echo "--- what the router logged"
(cd "$main" && "$H" process logs hydra-router -p "$pc_port" -n 60 2>/dev/null)
