#!/usr/bin/env bash
# Demo: a TLS front.
#
# Main's frontend runs in a mode that only accepts https addresses (a
# production build, say), and the database speaks plain HTTP. `tls: 22443` on
# the database has hydra's router accept TLS there and pass it on, with a
# certificate signed by a local CA that hydra makes; `baseline_value` hands
# that address to main's frontend only.
#
#  1. Main's frontend reaches the database through https; a worktree's own
#     frontend keeps the plain address.
#  2. A client that trusts hydra's CA gets through the front; one that doesn't
#     is refused. `hydra status` says when this machine is the second kind.
#  3. With the router stopped, `hydra status` says the front is down.
#
# Everything happens in a throwaway repo under $TMPDIR, with a CA of its own:
# it never touches the one `hydra trust` installs. Each step checks what it
# printed and the script exits non-zero when one is off.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
(cd "$root" && go build -o bin/ ./cmd/...)
H="$root/bin/hydra"
pc_port=${PC_PORT:-8197}
unset PC_PORT_NUM PC_ADDRESS

repo="$(mktemp -d "${TMPDIR:-/tmp}/hydra-tls.XXXXXX")"
repo="$(cd "$repo" && pwd -P)"
main="$repo/main"
export HYDRA_TLS_DIR="$repo/ca"
cleanup() {
  (cd "$repo/wt" 2>/dev/null && "$H" down >/dev/null 2>&1) || true
  (cd "$main" 2>/dev/null && "$H" down -p "$pc_port" >/dev/null 2>&1) || true
  rm -rf "$repo"
}
trap cleanup EXIT

g() { git -c user.name=demo -c user.email=demo@example.com -c commit.gpgsign=false "$@"; }
show() { sed -e "s#$repo#<repo>#g"; }
fail() { echo "FAILED: $*" >&2; exit 1; }
expect() { grep -qE -- "$2" <<<"$1" || fail "expected /$2/ in:"$'\n'"$1"; }
run() { set +e; out="$("$@" 2>&1)"; status=$?; set -e; }
wait_for() { for _ in $(seq 100); do curl -sf "localhost:$1" >/dev/null 2>&1 && return 0; sleep 0.1; done; fail "nothing answered on :$1"; }
db_url() { python3 -c 'import json, sys; u = json.load(sys.stdin)["upstreams"]["DB_URL"]; print(u["url"], "->", (u.get("reply") or {}).get("service", "ERROR " + str(u.get("error"))))'; }

mkdir -p "$main" && cd "$main"
g init -q -b main
sed "s#command: bin/mocksvc#command: $root/bin/mocksvc#" "$here/tls.yaml" >process-compose.yaml
printf '.process-compose/\n' >.gitignore
mkdir -p apps/web && echo page >apps/web/page.tsx
g add -A; g commit -qm init

echo "--- 1. main's frontend goes through the front; a worktree's doesn't"
"$H" up -D -p "$pc_port" -L "$repo/main.log" >/dev/null
wait_for 22000
for _ in $(seq 50); do curl -sk https://127.0.0.1:22443/ >/dev/null 2>&1 && break; sleep 0.1; done
out="$(curl -s localhost:22000 | db_url)"; echo "main's web:      DB_URL = $out"
expect "$out" '^https://127.0.0.1:22443 -> db$'
g worktree add -q -b wt "$repo/wt" main
(cd "$repo/wt" && echo edit >>apps/web/page.tsx)
cd "$repo/wt"
"$H" up -D -L "$repo/wt.log" >/dev/null 2>&1
wait_for 22100
out="$(curl -s localhost:22100 | db_url)"; echo "a worktree's:    DB_URL = $out"
expect "$out" '^http://127.0.0.1:22030 -> db$'

echo
echo "--- 2. who gets through"
out="$(curl -s --cacert "$HYDRA_TLS_DIR/ca.pem" https://127.0.0.1:22443/ | python3 -c 'import json, sys; r = json.load(sys.stdin); print(r["service"], "on its plain port", r["port"])')"
echo "with hydra's CA:    $out"
expect "$out" '^db on its plain port 22030$'
run curl -s https://127.0.0.1:22443/
echo "without it:         curl exits $status (60: the certificate isn't trusted)"
[ "$status" -ne 0 ] || fail "a client without the CA got through"
cd "$main"
run "$H" status; echo "$out" | grep -A1 -E '^(✗|!)' | show
[ "$status" -eq 0 ] || fail "an untrusted CA is a note, not an error: $out"
expect "$out" "this machine doesn't trust the certificate of main's TLS fronts"
expect "$out" 'fix: hydra trust'

echo
echo "--- 3. the router stops"
"$H" process stop hydra-router -p "$pc_port" >/dev/null 2>&1
sleep 1
run "$H" status; echo "$out" | grep -A1 -E '^✗' | show
[ "$status" -ne 0 ] || fail "status should fail with the front down"
expect "$out" "main's TLS front for db isn't listening on :22443"
"$H" process start hydra-router -p "$pc_port" >/dev/null 2>&1
for _ in $(seq 50); do curl -sk https://127.0.0.1:22443/ >/dev/null 2>&1 && break; sleep 0.1; done
run "$H" status; [ "$status" -eq 0 ] || fail "status should pass again: $out"
echo "started again: $(echo "$out" | tail -1)"
expect "$out" 'ok, with the notes above'
echo
echo "all checks passed"
