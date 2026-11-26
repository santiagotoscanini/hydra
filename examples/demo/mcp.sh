#!/usr/bin/env bash
# Demo: an agent drives its worktree's stack through `hydra mcp`.
#
# The script plays the agent harness: it starts `hydra mcp` in a worktree,
# as Claude Code or Codex would, and talks MCP to it (JSON-RPC, one message
# per line) on a pipe.
#
#  1. Main's stack runs. A worktree changed the API and a worker.
#  2. initialize, tools/list.
#  3. status, up, wait, status: the worktree's stack starts and is ready.
#  4. logs (with a filter), restart and wait for one process.
#  5. up with a service that is off by default: the worker, which fails.
#     wait says so, with its last lines.
#  6. main_status: what main runs, and what of it this worktree uses.
#  7. update_here (dry run), down.
#  8. In the main checkout, every action is refused, and main's stack is
#     still up afterwards; status and logs read it.
#  9. Updating main and every worktree, as the update_all prompt does it:
#     worktrees lists them in order (a branch after the one it is stacked
#     on), update_main pulls main from origin and restarts what changed in
#     its stack, update_worktree merges each worktree in order; one
#     conflicts, is resolved with git here, and updated again. prompts/get
#     returns the workflow.
# 10. Nothing but protocol messages ever reached stdout.
#
# Everything happens in a throwaway repo under $TMPDIR. Each step checks what
# it got back, so this doubles as a test: it exits non-zero when one is off.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
pc_port=${PC_PORT:-8196} # main's process-compose server; 8099 may be yours
unset PC_PORT_NUM PC_ADDRESS

repo="$(mktemp -d "${TMPDIR:-/tmp}/hydra-mcp.XXXXXX")"
repo="$(cd "$repo" && pwd -P)"
main="$repo/main"
# Built into the throwaway directory, not the checkout's bin/: with
# `make link`, that one is the hydra on your PATH.
(cd "$root" && go build -o "$repo/bin/" ./cmd/...)
H="$repo/bin/hydra"
srv=""
cleanup() {
  cd /
  stop_server
  for wt in feature ind; do
    (cd "$repo/$wt" 2>/dev/null && "$H" down >/dev/null 2>&1) || true
    (cd "$main" 2>/dev/null && "$H" slots free "$repo/$wt" >/dev/null 2>&1) || true
  done
  (cd "$main" 2>/dev/null && "$H" down -p "$pc_port" >/dev/null 2>&1) || true
  rm -rf "$repo"
}
trap cleanup EXIT

g() { git -c user.name=demo -c user.email=demo@example.com -c commit.gpgsign=false "$@"; }
put() { mkdir -p "$(dirname "$1")"; echo "${2:-$1}" >"$1"; }
show() { sed -e "s#$repo#<repo>#g" -e 's/[0-9a-f]\{9\}/<commit>/g'; }
fail() { echo "FAILED: $*" >&2; [ -s "$repo/err" ] && { echo "--- hydra mcp's stderr:" >&2; cat "$repo/err" >&2; }; exit 1; }
expect() { # text pattern
  grep -qE -- "$2" <<<"$1" || fail "expected /$2/ in:"$'\n'"$1"
}
wait_for() { for _ in $(seq 100); do curl -sf "localhost:$1" >/dev/null 2>&1 && return 0; sleep 0.1; done; fail "nothing answered on :$1"; }

# The harness: `hydra mcp` in a checkout, reading a pipe. Requests go one at
# a time, each once the last was answered, the way an agent calls tools.
start_server() { # checkout
  rm -f "$repo/in"; mkfifo "$repo/in"
  : >"$repo/out"; : >"$repo/err"
  (cd "$1" && exec "$H" mcp <"$repo/in" >"$repo/out" 2>"$repo/err") &
  srv=$!
  exec 3>"$repo/in"
  n=0
}
stop_server() {
  [ -n "$srv" ] || return 0
  exec 3>&- # end of input: the server stops
  for _ in $(seq 100); do kill -0 "$srv" 2>/dev/null || break; sleep 0.1; done
  kill -0 "$srv" 2>/dev/null && fail "hydra mcp didn't stop when its input closed"
  wait "$srv" || fail "hydra mcp exited with status $?"
  srv=""
  check_stdout
}
# rpc METHOD PARAMS: the reply goes in $reply. (Not run in $(...): the
# request ids count up in this shell.)
rpc() {
  n=$((n + 1))
  printf '{"jsonrpc":"2.0","id":%d,"method":"%s","params":%s}\n' "$n" "$1" "$2" >&3
  for _ in $(seq 1800); do
    if reply="$(grep -E "^\{\"id\":$n," "$repo/out")"; then
      return 0
    fi
    sleep 0.1
  done
  fail "no reply to $1 (id $n) after 3 minutes"
}
notify() { printf '{"jsonrpc":"2.0","method":"%s"}\n' "$1" >&3; }
# call TOOL [ARGS]: the tool's text goes in $out, with "ERROR: " in front
# when the result is an error. The JSON is in $reply.
call() {
  local args="${2:-}"
  [ -n "$args" ] || args='{}'
  rpc tools/call "{\"name\":\"$1\",\"arguments\":$args}"
  out="$(python3 -c '
import json, sys
r = json.loads(sys.argv[1])["result"]
print(("ERROR: " if r.get("isError") else "") + r["content"][0]["text"])
' "$reply")"
}
# Every line on stdout is a JSON-RPC 2.0 response: nothing else got there.
check_stdout() {
  python3 - "$repo/out" <<'PY' || fail "hydra mcp wrote something other than protocol messages to stdout"
import json, sys
for i, line in enumerate(open(sys.argv[1]), 1):
    try:
        m = json.loads(line)
        assert m["jsonrpc"] == "2.0" and "id" in m and ("result" in m or "error" in m)
    except Exception as e:
        print(f"line {i}: {line!r}: {e}", file=sys.stderr)
        sys.exit(1)
PY
}

g init -q -b main "$main"
cd "$main"
git config commit.gpgsign false; git config user.name demo; git config user.email demo@example.com
sed "s#command: bin/mocksvc#command: $repo/bin/mocksvc#" "$here/mcp.yaml" >process-compose.yaml
printf '.process-compose/\n' >.gitignore
put apps/api/users.go; put apps/web/page.tsx; put apps/worker/main.go
g add -A; g commit -qm init

echo "--- 1. main's stack, and a worktree that changed the api and the worker"
"$H" up -D -p "$pc_port" -L "$repo/main.log" >/dev/null
wait_for 24010
g worktree add -q -b feature "$repo/feature" main
(cd "$repo/feature" && put apps/api/orders.go && put apps/worker/queue.go && g add -A && g commit -qm "api: orders")

echo "--- 2. the harness starts hydra mcp in the worktree"
start_server "$repo/feature"
rpc initialize '{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"demo","version":"1"}}'; out="$reply"
expect "$out" '"protocolVersion":"2025-06-18"'
expect "$out" '"name":"hydra"'
notify notifications/initialized
rpc tools/list '{}'; out="$reply"
tools="$(python3 -c 'import json,sys; print(" ".join(t["name"] for t in json.loads(sys.argv[1])["result"]["tools"]))' "$out")"
echo "tools: $tools"
expect "$tools" '^status up wait down restart logs update_here main_status worktrees update_main update_worktree$'

echo "--- 3. status, up, wait"
call status; echo "$out" | show
expect "$out" 'stack not started'
expect "$out" 'api  here'
call up; echo "$out" | show
expect "$out" "this worktree's stack is up"
call wait '{"timeout_seconds":60}'; echo "$out" | show
expect "$out" '^ready after'
call status; echo "$out" | show
expect "$out" 'stack up'
expect "$out" 'api  here :24[0-9]10  ready'
expect "$out" 'db  main :24030  running'
slot="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["result"]["structuredContent"]["worktree"]["slot"])' "$reply")"
wait_for $((24010 + slot * 100))

echo "--- 4. logs, restart"
call logs '{"process":"api","contains":"listening"}'; echo "$out" | show
expect "$out" "api \(slot $slot\) listening on :$((24010 + slot * 100))"
call restart '{"process":"api"}'; echo "$out" | show
expect "$out" 'restarted api'
call wait '{"processes":["api"],"timeout_seconds":60}'; echo "$out" | show
expect "$out" '^ready after'
call wait '{"processes":["nope"]}'
expect "$out" '^ERROR: no process named nope'

echo "--- 5. the worker, off by default, started here: it fails, and wait says why"
call up '{"services":["worker"]}'; echo "$out" | show
expect "$out" 'hydra: (started )?worker (runs )?here'
call wait '{"processes":["worker"],"timeout_seconds":60}'; echo "$out" | show
expect "$out" '^not ready after .*: worker failed'
expect "$out" 'fatal: the queue refused the connection'

echo "--- 6. main_status"
call main_status; echo "$out" | show
expect "$out" 'stack up'
expect "$out" 'db  running  \(api here calls it\)'

echo "--- 7. update_here (dry run), down"
call update_here '{"dry_run":true}'; echo "$out" | show
expect "$out" 'dry run'
call down; echo "$out" | show
call status
expect "$out" 'stack not started'
stop_server

echo "--- 8. in the main checkout, actions are refused; reads work"
start_server "$main"
rpc initialize '{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"demo","version":"1"}}'
expect "$reply" '"protocolVersion":"2025-03-26"'
notify notifications/initialized
for tool in down up update_here; do
  call $tool
  expect "$out" "^ERROR: refused: this is the main checkout .*Ask the developer: main's stack is theirs"
done
call restart '{"process":"api"}'
expect "$out" "^ERROR: refused"
echo "$out" | show
call status; echo "$out" | show
expect "$out" 'stack up'
call logs '{"process":"api","lines":1}'; echo "$out" | show
expect "$out" 'api \(slot 0\) listening on :24010'
stop_server
curl -sf localhost:24010 >/dev/null || fail "main's api stopped"
echo "main's stack is still up"

echo "--- 9. update main and every worktree"
# origin, which main pulls from, and a teammate who pushes to it.
g init -q --bare -b main "$repo/origin.git"
g -C "$main" remote add origin "$repo/origin.git"
g -C "$main" push -q -u origin main
g clone -q "$repo/origin.git" "$repo/team" 2>/dev/null
# Worktrees: s1, s2 stacked on it, and ind, whose change to the API
# conflicts with the teammate's. ind's stack runs.
g -C "$main" worktree add -q -b s1 "$repo/s1" main
(cd "$repo/s1" && put apps/web/s1.tsx && g add -A && g commit -qm s1)
g -C "$main" worktree add -q -b s2 "$repo/s2" s1
(cd "$repo/s2" && put apps/web/s2.tsx && g add -A && g commit -qm s2)
g -C "$main" worktree add -q -b ind "$repo/ind" main
(cd "$repo/ind" && put apps/api/users.go "ind's users" && g add -A && g commit -qm ind && "$H" up -D >/dev/null)
(cd "$repo/team" && put apps/api/users.go "users v2" && g add -A && g commit -qm "api: users v2" && g push -q origin main)

start_server "$main" # the update tools work from main too
rpc initialize '{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"demo","version":"1"}}'
expect "$reply" '"prompts":'
notify notifications/initialized
rpc prompts/list '{}'
expect "$reply" '"name":"update_all"'
rpc prompts/get '{"name":"update_all"}'
expect "$reply" 'Call worktrees'
expect "$reply" 'update_main with dry_run: true'
call worktrees; echo "$out" | show
expect "$out" '1\. main +on main at .* · stack up'
expect "$out" '2\. feature +on main · 1 ahead · nothing to do'
expect "$out" '3\. ind +on main · 1 ahead · nothing to do · stack up \(slot [0-9]\)'
expect "$out" '4\. s1 +on main · 1 ahead'
expect "$out" '5\. └ s2 +on s1 · 1 ahead'
call update_main '{"dry_run":true}'; echo "$out" | show
expect "$out" 'would move .* \(1 commit\)'
expect "$out" 'api +would be restarted'
call update_main; echo "$out" | show
expect "$out" 'main  .*moved .* \(1 commit\)'
expect "$out" 'api +restarted'
expect "$out" 'ok: main is up to date. The worktrees were left alone'
curl -sf localhost:24010 >/dev/null || fail "main's api isn't back after the update"
call worktrees; echo "$out" | show
expect "$out" 'ind +on main · 1 behind, 1 ahead · CONFLICTS in 1 file: apps/api/users.go'
for wt in feature ind s1 s2; do
  call update_worktree "{\"worktree\":\"$wt\"}"; echo "$out" | show
  case $wt in
  ind)
    expect "$out" '^ERROR: hydra update --here failed'
    expect "$out" 'ind  ind  CONFLICTS with main in 1 file'
    expect "$out" 'The worktree was left as it was'
    [ -z "$(g -C "$repo/ind" status --porcelain)" ] || fail "ind was left changed"
    # The agent's part: merge by hand, resolve, commit, update again.
    before="$(g -C "$repo/ind" rev-parse HEAD)"
    g -C "$repo/ind" merge -q main >/dev/null 2>&1 && fail "the merge didn't conflict"
    echo "users v2, and ind's" >"$repo/ind/apps/api/users.go"
    g -C "$repo/ind" add apps/api/users.go
    g -C "$repo/ind" commit -q --no-edit >/dev/null 2>&1 # (rerere, if on, would say so)
    call update_worktree "{\"worktree\":\"ind\",\"since\":\"$before\"}"; echo "$out" | show
    expect "$out" 'up to date'
    expect "$out" 'restarted there: api'
    ;;
  s2) expect "$out" 's1 merged in' ;;
  *) expect "$out" 'main merged in' ;;
  esac
done
call worktrees; echo "$out" | show
for wt in feature ind s1 s2; do
  expect "$out" " $wt +on (main|s1) · [0-9]+ ahead · nothing to do"
done
stop_server

echo "--- 10. stdout carried protocol messages only"
echo ok
