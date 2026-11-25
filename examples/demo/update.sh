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
#     itself, frees its slot and leaves no folder behind, also when it lay
#     inside the main checkout and git left a folder there.
#  9. What an edit adds to a stack that is up starts in order: a service
#     after the step it needs first, and after the service it waits for,
#     also when that one only just moved into the worktree. Then git gives
#     up halfway through removing that worktree, and hydra finishes the job.
# 10. A process-compose that is killed doesn't take its processes with it.
#     The stack's watcher stops them and goes. Killed along with it, they
#     live on: `hydra down` there stops them, and so does the next
#     `hydra up`, before it starts anything. When process-compose is started
#     without hydra, `hydra status` names the one that holds a port, and the
#     restart it suggests clears it. And a worktree removed in that state is
#     cleaned up after like any other.
# 11. With main's stack down, a worktree won't start.
# 12. `hydra down --worktrees` stops every worktree's stack, also what a
#     killed process-compose left of one.
#
# Along the way it reads `hydra status --json` (what the dashboard shows)
# for the same facts.
#
# Everything happens in a throwaway repo under $TMPDIR. Each step checks what
# it printed, so this doubles as a test: it exits non-zero when one is off.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
pc_port=${PC_PORT:-8198} # main's process-compose server; 8099 may be yours
unset PC_PORT_NUM PC_ADDRESS

repo="$(mktemp -d "${TMPDIR:-/tmp}/hydra-update.XXXXXX")"
repo="$(cd "$repo" && pwd -P)"
main="$repo/main"
# Built into the throwaway directory, not the checkout's bin/: with
# `make link`, that one is the hydra on your PATH.
(cd "$root" && go build -o "$repo/bin/" ./cmd/...)
H="$repo/bin/hydra"
cleanup() {
  cd / # not from inside what is about to be cleared
  for wt in api web conflict dirty fresh order; do
    (cd "$repo/$wt" 2>/dev/null && "$H" down >/dev/null 2>&1) || true
  done
  # A freed slot takes the worktree's socket and note out of $TMPDIR.
  for wt in api web conflict dirty fresh order landed solo; do
    (cd "$main" 2>/dev/null && "$H" slots free "$repo/$wt" >/dev/null 2>&1) || true
  done
  (cd "$main" 2>/dev/null && "$H" down -p "$pc_port" >/dev/null 2>&1) || true
  # Step 10 kills process-compose on purpose; whatever that left running
  # from this directory goes with it.
  lsof -a -d cwd -Fpn 2>/dev/null | awk -v repo="$repo" '/^p/{pid=substr($0,2)} /^n/{if (index(substr($0,2), repo) == 1) print pid}' | xargs kill 2>/dev/null || true
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
holder() { # the process listening on a port, and the one that started it
  local pid; pid="$(lsof -nP -iTCP:"$1" -sTCP:LISTEN -t 2>/dev/null | head -1)"
  [ -n "$pid" ] && ps -o pid=,ppid=,command= -p "$pid" -p "$(ps -o ppid= -p "$pid" | tr -d ' ')" 2>/dev/null | tr '\n' ';'
}
# A removed worktree is cleaned up after when its folder is gone and its slot
# is free. hydra does that by itself, a moment after the removal.
cleaned() {
  local taken; taken="$(cd "$main" && "$H" slots)" # not piped into grep -q: with pipefail, a match would read as a failure
  [ ! -e "$1" ] && ! grep -qF " $1" <<<"$taken"
}
wait_cleaned() {
  for _ in $(seq 300); do cleaned "$1" && return 0; sleep 0.1; done
  fail "$1 was removed, and after 30s its folder or its slot is still there:"$'\n'"$(ls -A "$1" 2>&1; cd "$main" && "$H" slots)"
}
wait_for() { for _ in $(seq 100); do curl -sf "localhost:$1" >/dev/null 2>&1 && return 0; sleep 0.1; done; fail "nothing answered on :$1"; }

# origin, the main checkout, and a clone that stands in for teammates.
g init -q --bare -b main "$repo/origin.git"
g clone -q "$repo/origin.git" "$main" 2>/dev/null
cd "$main"
git config commit.gpgsign false; git config user.name demo; git config user.email demo@example.com
sed "s#command: bin/mocksvc#command: $repo/bin/mocksvc#" "$here/update.yaml" >process-compose.yaml
printf '.process-compose/\n.wt/\n' >.gitignore
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

# The same as JSON, what the dashboard (bare `hydra`) reads: every checkout,
# from any checkout.
page="$(cd "$main" && "$H" status --json | python3 -c '
import json, sys
d = json.load(sys.stdin)
wt = {w["branch"]: w for w in d["worktrees"]}["api"]
svc = {s["name"]: s for s in wt["services"]}
print("main up:", d["main"]["up"], "| processes:", len(d["main"]["processes"]))
print("api worktree: slot", wt["slot"], "| up:", wt["up"], "| problems:", len(wt["problems"]), "| update:", wt["update"]["note"])
print("api runs", svc["api"]["runs"], "on", svc["api"]["port"], svc["api"]["state"], "| web is", svc["web"]["runs"], "on", svc["web"]["port"], "| db is", svc["db"]["runs"])
')"
echo "hydra status --json says:"; echo "$page" | sed 's/^/  /'
expect "$page" 'main up: True'
expect "$page" "api worktree: slot $slot \| up: True \| problems: 0 \| update: up to date"
expect "$page" "api runs here on $((21010 + slot * 100)) ready \| web is routed on $((21000 + slot * 100)) \| db is main"
# --watch prints one a second, and reads a line in to look again now.
watched="$( (echo refresh; sleep 2.5) | "$H" status --json --watch | wc -l | tr -d ' ')"
[ "$watched" -ge 3 ] || fail "status --json --watch printed $watched snapshots in 2.5s"

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
g worktree add -q -b solo "$repo/solo" main
(cd "$repo/solo" && put apps/web/solo.tsx && g add -A && g commit -qm "web: solo")

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
echo "one worktree on its own: hydra update --here merges main into it, and moves nothing else"
cd "$repo/conflict"
run "$H" update --here --dry-run; echo "$out" | show
refuse "$out" 'conflict +conflict +CONFLICTS with main in 1 file'
cd "$repo/solo"
run "$H" update --here; echo "$out" | show
[ "$status" -eq 0 ] || fail "update --here should succeed"
expect "$out" 'solo +solo +main merged in \(4 commits\)'
expect "$out" "main itself wasn't pulled"
run "$H" status --all
expect "$out" "main's files moved from .* without .hydra update." # main's stack was left alone
cd "$main"
run "$H" update --here
refuse "$out" '--here is for a worktree'

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
expect "$out" 'fix: hydra update --here +\(or, for every worktree: hydra update\)'
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
# git may give up before the folder is empty: the stack still writes there.
cd "$main"; g worktree remove --force "$repo/api" 2>/dev/null || true
wait_cleaned "$repo/api"
curl -sf "localhost:$api_port" >/dev/null 2>&1 && fail "the removed worktree's api still answers on :$api_port: $(holder "$api_port")"
echo "the removed worktree's stack stopped by itself, and its slot is free again:"
run "$H" slots; echo "$out" | show
# The same for a worktree kept inside the main checkout, of which git left a
# folder without its .git. git takes that folder for a part of main now, and
# nothing that still runs there may act on that.
g worktree add -q -b inner "$main/.wt/inner" main
cd "$main/.wt/inner"; put apps/api/inner.go
run "$H" up -D -L "$repo/inner.log"; [ "$status" -eq 0 ] || fail "inner should start: $out"
run "$H" status --wait; [ "$status" -eq 0 ] || fail "inner should be up: $out"
inner_port=$((21010 + $("$H" slots | awk '$1=="*"{print $2}') * 100))
cd "$main"; rm -rf "$main/.wt/inner/.git" "$main/.wt/inner/apps/web" "$main/.git/worktrees/inner"
wait_cleaned "$main/.wt/inner"
curl -sf "localhost:$inner_port" >/dev/null 2>&1 && fail "the api of the worktree inside main still answers on :$inner_port: $(holder "$inner_port")"
run "$H" status; [ "$status" -eq 0 ] || fail "main's stack should be as it was: $out"
grep -q hydra-router "$repo/inner.log" && fail "the removed worktree's stack was handed main's processes: $(grep hydra-router "$repo/inner.log" | head -3)"
echo "so did the stack of a worktree inside main, of which git had left a folder"
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
# With all of that running, git gives up halfway through removing the
# worktree: it forgets it, and leaves most of the folder, .git file and all.
rm -rf "$main/.git/worktrees/order" "$repo/order/apps/web"
wait_cleaned "$repo/order"
echo "removed halfway by git with seven services up: hydra stopped them and deleted the rest"

echo
echo "--- 10. process-compose is killed"
cd "$repo/web"
web_port=$((21000 + $("$H" slots | awk '$1=="*"{print $2}') * 100))
pc_of() { pgrep -f "process-compose up -f $repo/$1/" | head -1; }
web_pc() { pc_of web; }
alive() { local n=0 p; for p in $1; do kill -0 "$p" 2>/dev/null && n=$((n + 1)); done; echo "$n"; }
gone() { # every process named is gone, within ten seconds
  for _ in $(seq 100); do [ "$(alive "$1")" = 0 ] && return 0; sleep 0.1; done
  fail "still running: $(ps -o pid=,ppid=,command= -p "$(tr ' ' ',' <<<"${1% }")")"
}
kill_both() { # a worktree's process-compose and its watcher at once, as `pkill -f process-compose` does
  local watcher; watcher="$(pgrep -f "hydra watch --root $repo/$1 " | head -1)"
  kill -STOP "$watcher"; kill -9 "$(pc_of "$1")" "$watcher" # stopped first, so it can't act in between
  sleep 1
}
kill_web() {
  kill_both web
  curl -sf "localhost:$web_port" >/dev/null 2>&1 || fail "web didn't outlive its process-compose, so there is nothing to show"
}
pids() { sed 's/pid [0-9]*/pid <pid>/g'; }
pc_cmd="$(ps -o command= -p "$(web_pc)")" # how hydra started it, for later
sleep 3 # the watcher writes down what process-compose runs every two seconds

# Killed alone: the watcher notices, stops what it left, and goes.
kids="$(pgrep -P "$(web_pc)" | tr '\n' ' ')"
kill -9 "$(web_pc)"
gone "$kids"
curl -sf "localhost:$web_port" >/dev/null 2>&1 && fail "web still answers on :$web_port: $(holder "$web_port")"
echo "killed alone: the watcher stopped the $(wc -w <<<"$kids" | tr -d ' ') processes it had started, itself among them"
run "$H" status; echo "$out" | grep -A1 '^✗' | show
refuse "$out" "this worktree's stack isn't running"
run "$H" up -D -L "$repo/web.log"; [ "$status" -eq 0 ] || fail "web should start again: $out"
grep -q '^hydra: stopped' <<<"$out" && fail "nothing should have been left to stop: $out"
run "$H" status --wait; echo "$out" | tail -1 | show
[ "$status" -eq 0 ] || fail "web should be up again: $out"

# Killed with its watcher: what it had started lives on. `hydra down` there
# stops it, by what the watcher wrote down.
sleep 3
kids="$(pgrep -P "$(web_pc)" | tr '\n' ' ')"
kill_web
run "$H" down; echo "$out" | pids | show
expect "$out" 'hydra: stopped what an earlier run of this stack left running: .*web \(pid [0-9]+\)'
gone "$kids"
run "$H" down; echo "$out" | show
expect "$out" "this checkout's stack isn't running; nothing to stop"

# And so does the next start, before it starts anything.
run "$H" up -D -L "$repo/web.log"; [ "$status" -eq 0 ] || fail "web should start: $out"
run "$H" status --wait; [ "$status" -eq 0 ] || fail "web should be up: $out"
sleep 3
kids="$(pgrep -P "$(web_pc)" | tr '\n' ' ')"
kill_web
run "$H" up -D -L "$repo/web.log"; echo "$out" | grep '^hydra: stopped' | pids | show
[ "$status" -eq 0 ] || fail "web should start again: $out"
expect "$out" 'hydra: stopped what an earlier run of this stack left running: .*web \(pid [0-9]+\)'
gone "$kids"
run "$H" status --wait; echo "$out" | tail -1 | show
[ "$status" -eq 0 ] || fail "web should be up again without anything else: $out"

# Started without hydra, process-compose runs into the leftover. `hydra
# status` says what holds the port, and the restart it names clears it.
kill_web
$pc_cmd >/dev/null 2>&1 &
disown # it is killed further down, and the shell would report that
for _ in $(seq 50); do "$H" process list >/dev/null 2>&1 && break; sleep 0.1; done
run "$H" status --wait; echo "$out" | sed -n '/^✗/,$p' | sed -e 's#^\(      | \)[0-9/]* [0-9:]* #\1#' | pids | show
refuse "$out" 'Port [0-9]+ is held by mocksvc \(pid [0-9]+\), left over from an earlier run of this stack'
fix="$(sed -n 's/^  fix: //p' <<<"$out" | head -1)"
expect "$fix" '^hydra process restart web   \(stops the leftover first\)$'
run "$H" process restart web; echo "$out" | grep '^hydra: stopped' | pids | show
expect "$out" 'hydra: stopped what an earlier run of this stack left running: web \(pid [0-9]+\)'
run "$H" status --wait; echo "$out" | tail -1 | show
[ "$status" -eq 0 ] || fail "web should be up after the fix: $out"

# Killed with its watcher and then removed: nobody is left to notice. The
# next start anywhere finishes the job, here one of a stack that is up.
sleep 3
kids="$(pgrep -P "$(web_pc)" | tr '\n' ' ')"
kill_web
cd "$main"; g worktree remove --force "$repo/web" 2>/dev/null || true
run "$H" up -D -p "$pc_port" -L "$repo/main.log"; echo "$out" | grep 'was removed' | show
expect "$out" 'hydra: web was removed: stopped what its stack had left running \(.*web.*\).*slot [0-9]+ is free again'
gone "$kids"
cleaned "$repo/web" || fail "the removed worktree's folder or slot is still there: $(ls -A "$repo/web" 2>&1; "$H" slots)"

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
echo "--- 12. the end of the day"
# One worktree's stack is still up, and it loses its process-compose and its
# watcher. `hydra down --worktrees` stops what they left like any stack.
kids="$(pgrep -P "$(pc_of fresh)" | tr '\n' ' ')"
kill_both fresh
cd "$main"
run "$H" down --worktrees; echo "$out" | pids | show
[ "$status" -eq 0 ] || fail "down --worktrees: $out"
expect "$out" "fresh: its process-compose was gone, part of its stack wasn't"
expect "$out" '  hydra: stopped what an earlier run of this stack left running: .*api \(pid [0-9]+\)'
expect "$out" 'stopped 1 worktree stack'
gone "$kids"
run "$H" down --worktrees; echo "$out" | show
expect "$out" "no worktree's stack is running"

echo
echo "--- 13. the journal"
# Everything above went into one journal for main and every worktree.
run "$H" log --since 1h --json
for ev in command problem slot start process log; do
  expect "$out" "\"ev\":\"$ev\""
done
expect "$out" '"ev":"command","checkout":"main".*"args":\["down","--worktrees"\]'
expect "$out" '"ev":"process","checkout":"main".*"name":"api"'
expect "$out" '"ev":"problem".*"level":"error","text":"main.s stack isn.t running'
run "$H" log --since 1h --errors; echo "$out" | tail -3 | show
expect "$out" "✗ main's stack isn't running"
! grep -q ' usage ' <<<"$out" || fail "--errors listed usage samples: $out"
run "$H" report --since 1h
report="$(echo "$out" | tail -1)"
[ -f "$report" ] || fail "hydra report wrote no file: $out"
for h in '# hydra report' '## Versions' '## `hydra status --all` (in main)' '## Commands' '## What went wrong' '## Journal'; do
  grep -qF -- "$h" "$report" || fail "the report has no '$h'"
done
expect "$(cat "$report")" "main's stack isn't running"
echo "report: $(wc -l <"$report" | tr -d ' ') lines"
echo
echo "all checks passed"
