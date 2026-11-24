#!/usr/bin/env bash
# Demo: keeping worktrees in step with main.
#
#  1. Main's stack runs; a worktree that changed the API starts on top of it
#     and `hydra status` says everything it uses is up.
#  2. Teammates push to origin. Someone pulls main by hand: `hydra status`
#     notices that main's stack still runs the old code.
#  3. `hydra update` runs the jobs again, restarts what the pull touched, in
#     order, and merges main into every worktree it can. It leaves alone the
#     ones with a conflict or uncommitted work, and says so.
#  4. A worktree cut from origin's newer main refuses to start on main's
#     older stack, and says what to run.
#  5. A migration that fails stops the update before the API restarts.
#  6. A process that is off by default and was started by hand restarts
#     with the rest when a pull touches its files.
#  7. A change to the stack file itself reaches the running stack.
#  8. Starting a stack that is up is not an error, and `hydra up NAME` adds
#     a process to it. `hydra status --wait` gives up on a process that never
#     gets ready, and says so. A worktree removed with its stack up stops by
#     itself, and its slot goes to the next worktree only after that.
#  9. What an edit adds to a stack that is up starts in order: a service
#     after the step it needs first, and after the service it waits for,
#     also when that one only just moved into the worktree.
# 10. A process-compose that is killed leaves its services holding their
#     ports; `hydra status` names the one in the way.
# 11. With main's stack down, a worktree won't start.
#
# Along the way it reads the status page (`hydra ui`) for the same facts.
#
# Everything happens in a throwaway repo under $TMPDIR. Each step checks what
# it printed, so this doubles as a test: it exits non-zero when one is off.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
pc_port=${PC_PORT:-8198} # main's process-compose server; 8099 may be yours
ui_port=${UI_PORT:-4898} # the status page; 4937 may be yours
unset PC_PORT_NUM PC_ADDRESS

repo="$(mktemp -d "${TMPDIR:-/tmp}/hydra-update.XXXXXX")"
repo="$(cd "$repo" && pwd -P)"
main="$repo/main"
# Built into the throwaway directory, not the checkout's bin/: with
# `make link`, that one is the hydra on your PATH.
(cd "$root" && go build -o "$repo/bin/" ./cmd/...)
H="$repo/bin/hydra"
cleanup() {
  [ -n "${ui_pid:-}" ] && kill "$ui_pid" 2>/dev/null
  for wt in api web conflict dirty fresh order; do
    (cd "$repo/$wt" 2>/dev/null && "$H" down >/dev/null 2>&1) || true
  done
  (cd "$main" 2>/dev/null && "$H" down -p "$pc_port" >/dev/null 2>&1) || true
  rm -rf "$repo"
}
trap cleanup EXIT

g() { git -c user.name=demo -c user.email=demo@example.com -c commit.gpgsign=false "$@"; }
put() { mkdir -p "$(dirname "$1")"; echo "${2:-$1}" >"$1"; }
show() { sed -e "s#$repo#<repo>#g" -e 's/[0-9a-f]\{9\}/<commit>/g' -e 's/ in [0-9]*s$//'; }
fail() { echo "FAILED: $*" >&2; exit 1; }
expect() { # text pattern
  grep -qE -- "$2" <<<"$1" || fail "expected /$2/ in:"$'\n'"$1"
}
refuse() { # text pattern: the command must have exited non-zero
  [ "$status" -ne 0 ] || fail "expected a non-zero exit"
  expect "$1" "$2"
}
run() { set +e; out="$("$@" 2>&1)"; status=$?; set -e; }
wait_for() { for _ in $(seq 100); do curl -sf "localhost:$1" >/dev/null 2>&1 && return 0; sleep 0.1; done; fail "nothing answered on :$1"; }

# origin, the main checkout, and a clone that stands in for teammates.
g init -q --bare -b main "$repo/origin.git"
g clone -q "$repo/origin.git" "$main" 2>/dev/null
cd "$main"
git config commit.gpgsign false; git config user.name demo; git config user.email demo@example.com
sed "s#command: bin/mocksvc#command: $repo/bin/mocksvc#" "$here/update.yaml" >process-compose.yaml
printf '.process-compose/\n' >.gitignore
put README; put lock.txt v1; put db/migrations/001.sql; put apps/api/users.go; put apps/web/page.tsx
g add -A; g commit -qm init; g push -q origin main
g clone -q "$repo/origin.git" "$repo/team" 2>/dev/null
team() { (cd "$repo/team" && g pull -q && "$@" && g add -A && g commit -qm "$*" && g push -q origin main); }

echo "--- 1. main's stack, and a worktree that changed the api"
"$H" up -D -p "$pc_port" -L "$repo/main.log" >/dev/null
wait_for 21000
g worktree add -q -b api "$repo/api" main
(cd "$repo/api" && put apps/api/orders.go && g add -A && g commit -qm "api: orders")
cd "$repo/api"
"$H" up -D -L "$repo/api.log" 2>&1 | grep '^hydra' | show
slot=$("$H" slots | awk '$1=="*"{print $2}')
wait_for $((21010 + slot * 100)); wait_for $((21000 + slot * 100))
echo "its web port: $(curl -s "localhost:$((21000 + slot * 100))" | python3 "$here/chain.py")"
run "$H" status --wait; echo "$out" | show
[ "$status" -eq 0 ] || fail "status should be clean"
expect "$out" 'ok: everything this worktree uses is up'

# The same as a web page: `hydra ui` serves it, from any checkout.
"$H" ui --no-open --port "$ui_port" >"$repo/ui.log" 2>&1 &
ui_pid=$!
wait_for "$ui_port"
page="$(curl -s "localhost:$ui_port/api/status" | python3 -c '
import json, sys
d = json.load(sys.stdin)
wt = {w["branch"]: w for w in d["worktrees"]}["api"]
svc = {s["name"]: s for s in wt["services"]}
print("main up:", d["main"]["up"], "| processes:", len(d["main"]["processes"]))
print("api worktree: slot", wt["slot"], "| up:", wt["up"], "| problems:", len(wt["problems"]))
print("api runs", svc["api"]["runs"], "on", svc["api"]["port"], svc["api"]["state"], "| web is", svc["web"]["runs"], "on", svc["web"]["port"], "| db is", svc["db"]["runs"])
')"
echo "the status page says:"; echo "$page" | sed 's/^/  /'
expect "$page" 'main up: True'
expect "$page" "api worktree: slot $slot \| up: True \| problems: 0"
expect "$page" "api runs here on $((21010 + slot * 100)) ready \| web is routed on $((21000 + slot * 100)) \| db is main"
expect "$(curl -s "localhost:$ui_port/")" '<title>hydra</title>'
run "$H" ui --no-open --port "$ui_port"   # a second one finds the first and steps aside
expect "$out" 'already up at http://127.0.0.1:'"$ui_port"

# More worktrees, in the states an update has to tell apart.
cd "$main"
g worktree add -q -b web "$repo/web" main
(cd "$repo/web" && put apps/web/cart.tsx && g add -A && g commit -qm "web: cart")
g worktree add -q -b conflict "$repo/conflict" main
(cd "$repo/conflict" && put apps/api/users.go "the branch's version" && g add -A && g commit -qm "api: users, the branch's way")
g worktree add -q -b dirty "$repo/dirty" main
(cd "$repo/dirty" && put apps/web/draft.tsx && g add -A && g commit -qm "web: draft" && echo wip >>apps/web/page.tsx)
g worktree add -q -b landed "$repo/landed" main
(cd "$repo/landed" && put docs/notes.md && g add -A && g commit -qm "docs: notes")

echo
echo "--- 2. teammates push; someone pulls main by hand"
team put apps/api/users.go "main's version" >/dev/null
team put db/migrations/002.sql >/dev/null
team put lock.txt v2 >/dev/null
team put docs/notes.md >/dev/null # what the 'landed' branch did, squash-merged
g pull -q --ff-only
run "$H" status --all; echo "$out" | show
refuse "$out" "main's files moved from .* without .hydra update."
expect "$out" 'api: apps/api/users.go'
expect "$out" 'migrate: db/migrations/002.sql'

echo
echo "--- 3. hydra update"
run "$H" update; echo "$out" | show
[ "$status" -eq 0 ] || fail "update should succeed"
expect "$out" 'deps +ran again +lock.txt'
expect "$out" 'migrate +ran again +db/migrations/002.sql'
expect "$out" 'api +restarted +apps/api/users.go +ready'
expect "$out" 'api +api +main merged in \(4 commits\); restarted there: api, deps'
expect "$out" 'web +web +main merged in'
expect "$out" 'conflict +conflict +CONFLICTS with main in 1 file \(4 commits behind\)'
expect "$out" 'conflict: apps/api/users.go'
expect "$out" 'resolve:  git -C .*/conflict merge main'
expect "$out" 'dirty +dirty +left alone, 4 commits behind main: 1 file with uncommitted changes'
expect "$out" 'landed +landed +left alone: everything on it is already in main'
expect "$(cat "$main/.process-compose/logs"/* 2>/dev/null; "$H" process logs migrate -p "$pc_port" 2>/dev/null | tail -3)" 'applied +2 migrations'
[ "$(g -C "$repo/dirty" status --porcelain | wc -l | tr -d ' ')" = 1 ] || fail "the dirty worktree was touched"
echo
echo "the conflicted worktree, which uses main's web for its api:"
cd "$repo/conflict"
run "$H" up -D -L "$repo/conflict.log"; echo "$out" | grep -v '^Starting' | show
refuse "$out" 'this branch is behind main'
expect "$out" 'migrate \(api here waits for it\): db/migrations/002.sql'
expect "$out" 'fix: hydra update +\(or, for this worktree alone: git merge main\)'
echo
echo "the api worktree, after the merge:"
cd "$repo/api"
run "$H" status --wait; echo "$out" | tail -2 | show
[ "$status" -eq 0 ] || fail "the api worktree should be in step again"

echo
echo "--- 4. a worktree cut from origin's main, which is ahead of the main checkout"
team put apps/web/page.tsx "a newer page" >/dev/null
cd "$main"; g fetch -q
g worktree add -q -b fresh "$repo/fresh" origin/main 2>/dev/null
(cd "$repo/fresh" && put apps/api/fresh.go && g add -A && g commit -qm "api: fresh")
cd "$repo/fresh"
run "$H" up -D -L "$repo/fresh.log"; echo "$out" | grep -v '^Starting' | show
refuse "$out" 'main is older than the commit this branch started from'
expect "$out" 'web \(it serves this worktree.s api\): apps/web/page.tsx'
run "$H" update --dry-run; echo "$out" | show
expect "$out" 'would move .* \(1 commit\)'
expect "$out" 'web +would be restarted +apps/web/page.tsx'
run "$H" update; [ "$status" -eq 0 ] || fail "update should succeed: $out"
run "$H" up -D -L "$repo/fresh.log"; [ "$status" -eq 0 ] || fail "fresh should start now: $out"
echo "after hydra update it starts: $(echo "$out" | grep 'running here' | show)"

echo
echo "--- 5. a migration that fails"
team put db/migrations/FAIL >/dev/null
team put apps/api/users.go "main's next version" >/dev/null
cd "$main"
run "$H" update --main-only; echo "$out" | show
refuse "$out" 'migrate +FAILED'
expect "$out" 'not restarted, because migrate failed: api'
run "$H" status; refuse "$out" "main's files moved from"
team rm -f db/migrations/FAIL >/dev/null
run "$H" update --main-only; echo "$out" | tail -4 | show
[ "$status" -eq 0 ] || fail "update should succeed once the migration is fixed"
expect "$out" 'api +restarted'

echo
echo "--- 6. a process that is off by default, started by hand"
"$H" process start agent -p "$pc_port" >/dev/null
for _ in $(seq 50); do "$H" process get agent -p "$pc_port" -o json | grep -qE '"status": *"Running"' && break; sleep 0.1; done
team put apps/agent/main.py >/dev/null
run "$H" update --main-only; echo "$out" | show
[ "$status" -eq 0 ] || fail "update should restart the agent and wait for its new run"
expect "$out" 'agent +restarted +apps/agent/main.py +running'
expect "$("$H" process get agent -p "$pc_port" -o json)" '"status": *"Running"'

echo
echo "--- 7. the stack file changes"
add_cache() { printf '\n  cache:\n    command: sleep 1000\n' >>process-compose.yaml; }
team add_cache >/dev/null
cd "$main"; g pull -q --ff-only
run "$H" status; echo "$out" | grep -A1 -E '^(✗|!)' | show
expect "$out" "main's stack was started from an older process-compose.yaml"
run "$H" update --main-only; echo "$out" | show
[ "$status" -eq 0 ] || fail "update should reload the configuration"
expect "$out" 'configuration reloaded .*: adds cache'
run "$H" status --wait; echo "$out" | grep -E '^(cache|ok)' | show
[ "$status" -eq 0 ] || fail "main should be clean after the reload: $out"
expect "$out" 'cache +running'
# What is off by default stays off through a reload and an update.
expect "$("$H" process get seed -p "$pc_port" -o json)" '"status": *"Disabled"'
# A stack hydra has no record for (started by an older hydra, say): an
# update can't tell what changed under it, so it restarts everything once.
# (Without the record, PC_PORT_NUM says where main's process-compose is.)
rm "$main/.git/hydra-baseline.json"
export PC_PORT_NUM=$pc_port
run "$H" status; expect "$out" 'hydra has no record of the commit'
run "$H" update --main-only; echo "$out" | grep -E 'no record' | show
[ "$status" -eq 0 ] || fail "update should succeed without a record: $out"
expect "$out" 'deps +ran again +\(no record of the commit it runs\)'
expect "$out" 'web +restarted +\(no record of the commit it runs\)'
expect "$("$H" process get seed -o json)" '"status": *"Disabled"'
unset PC_PORT_NUM
run "$H" status --wait; [ "$status" -eq 0 ] || fail "main should be clean again: $out"
expect "$out" "ok: main's stack is up and runs what main has checked out"

echo
echo "--- 8. a second start, one more process, and a worktree that is removed"
cd "$repo/api"
run "$H" up -D -L "$repo/api.log"; echo "$out" | show
[ "$status" -eq 0 ] || fail "starting a stack that is up is not an error"
expect "$out" "this checkout's stack is already up"
run "$H" up seed; echo "$out" | grep '^hydra' | show # off by default, and not changed here
[ "$status" -eq 0 ] || fail "hydra up seed: $out"
expect "$out" 'hydra: seed runs here'
run "$H" status --wait; expect "$out" 'seed +here +- +running'
# A process that never gets ready: the wait ends, and not with "ok".
run "$H" up stuck; expect "$out" 'hydra: (started stuck here|stuck runs here)'
run "$H" status --wait --timeout 3s; echo "$out" | grep -E '^(✗|!)' | show
refuse "$out" 'stuck here is still starting, after waiting 3s'
slot=$("$H" slots | awk '$1=="*"{print $2}')
api_port=$((21010 + slot * 100))
# Removed with its stack up, the way a tool that manages worktrees does it.
cd "$main"; g worktree remove --force "$repo/api"
for _ in $(seq 150); do curl -sf "localhost:$api_port" >/dev/null 2>&1 || break; sleep 0.1; done
curl -sf "localhost:$api_port" >/dev/null 2>&1 && fail "the removed worktree's api still answers on :$api_port"
echo "the removed worktree's stack stopped by itself; its slot is held until the next start:"
run "$H" slots; echo "$out" | sed 's/(removed.*/(removed; ...)/' | show
expect "$out" " $slot +$repo/api +\(removed"
cd "$repo/web"
g merge -q main # steps 5 to 7 moved main without the worktrees
run "$H" up -D -L "$repo/web.log"; echo "$out" | grep '^hydra' | show
[ "$status" -eq 0 ] || fail "web should start: $out"
expect "$out" "hydra: slot $slot, running here"
run "$H" slots; expect "$out" "\* +$slot +$repo/web"
grep -q removed <<<"$out" && fail "the removed worktree still holds a slot: $out"
run "$H" status --wait; [ "$status" -eq 0 ] || fail "web should be up: $out"

echo
echo "--- 9. what an edit adds to a running stack starts in order"
# process-compose starts what an update adds or changes in no order of its
# own, and a process whose dependency isn't in the project yet doesn't wait
# for it. hydra hands the new file over in dependency order instead.
cd "$main"; g worktree add -q -b order "$repo/order" main
cd "$repo/order"
put apps/ask/client.go
run "$H" up -D -L "$repo/order.log"; echo "$out" | grep '^hydra' | show
[ "$status" -eq 0 ] || fail "order should start: $out"
run "$H" status --wait; [ "$status" -eq 0 ] || fail "ask-* should be up on main's api: $out"
# One edit adds three services and the step each needs first: six processes,
# and none of the services starts before its step ended.
put apps/jobs/queue.go
run "$H" status --wait; echo "$out" | grep -E '^(jobs|✗)' | show
[ "$status" -eq 0 ] || fail "the services with a prepare step should be up: $out"
expect "$out" 'jobs-a +here +- +running'
expect "$out" 'jobs-b +here +- +running'
expect "$out" 'jobs-c +here +- +running'
# One more brings the api here. What waits for it is restarted on the new
# api's address, and only once that api is up.
put apps/api/local.go
run "$H" status --wait; echo "$out" | grep -E '^(api|ask|✗|ok)' | show
[ "$status" -eq 0 ] || fail "ask-* should have waited for the api that moved here: $out"
expect "$out" 'api +here +[0-9]+ +ready'
expect "$out" 'ask-a +here +- +running'
expect "$out" 'ask-b +here +- +running'
expect "$out" 'ask-c +here +- +running'
"$H" down >/dev/null 2>&1

echo
echo "--- 10. process-compose is killed, and its services live on"
cd "$repo/web"
kill -9 "$(pgrep -f "process-compose up -f $repo/web/" | head -1)"
sleep 1
run "$H" up -D -L "$repo/web.log"; [ "$status" -eq 0 ] || fail "web should start again: $out"
run "$H" status --wait; echo "$out" | sed -n '/^✗/,$p' | sed -e 's/pid [0-9]*/pid <pid>/' -e 's/kill [0-9]*/kill <pid>/' -e 's#^\(      | \)[0-9/]* [0-9:]* #\1#' | show
refuse "$out" 'Port [0-9]+ is held by mocksvc \(pid [0-9]+\), left over from an earlier run of this stack'
fix="$(sed -n 's/^  fix: //p' <<<"$out" | head -1)"
expect "$fix" '^kill [0-9]+ && hydra process restart web$'
kill "$(grep -oE '^kill [0-9]+' <<<"$fix" | cut -d' ' -f2)"; "$H" process restart web >/dev/null
run "$H" status --wait; echo "$out" | tail -1 | show
[ "$status" -eq 0 ] || fail "web should be up after the fix: $out"

echo
echo "--- 11. main's stack goes down"
(cd "$main" && "$H" down -p "$pc_port" >/dev/null 2>&1)
cd "$repo/dirty"
run "$H" up -D -L "$repo/dirty.log"; echo "$out" | show
refuse "$out" "main's stack isn't running"
cd "$repo/fresh"
run "$H" status; echo "$out" | grep -E '^(✗|  fix)' | show
refuse "$out" "main's stack isn't running"
sleep 3
expect "$(cat "$repo/fresh/.process-compose/logs"/* 2>/dev/null; "$H" process logs hydra-watch 2>/dev/null | tail -5)" "main's stack isn't answering"
echo
echo "all checks passed"
