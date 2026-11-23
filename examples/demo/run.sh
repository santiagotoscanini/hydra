#!/usr/bin/env bash
# Demo: start the main checkout's stack, then a "worktree" that changed the API and
# wants web-b (but not web-a) to go through its own API. Curl both frontends
# to see which API and database each request reached.
set -euo pipefail
cd "$(dirname "$0")"
here=$PWD
mkdir -p .hydra
# Built into the demo's own directory, not the checkout's bin/: with
# `make link`, that one is the hydra on your PATH.
(cd ../.. && go build -o "$here/.hydra/bin/" ./cmd/...)
H=$here/.hydra/bin/hydra
sed "s#command: bin/mocksvc.*#command: $here/.hydra/bin/mocksvc#" process-compose.yaml >.hydra/process-compose.yaml

sock_base=$PWD/.hydra/main.sock
sock_wt=$PWD/.hydra/slot1.sock
cleanup() {
  process-compose down -U -u "$sock_wt" >/dev/null 2>&1 || true
  process-compose down -U -u "$sock_base" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "--- main (slot 0): every service"
$H render -f .hydra/process-compose.yaml --slot 0 -o .hydra/main.yaml
process-compose up -D -U -u "$sock_base" -f .hydra/main.yaml -L .hydra/main.log

echo "--- worktree (slot 1): changed apps/api, run web-b here too"
$H plan   -f .hydra/process-compose.yaml --slot 1 --changed apps/api/handlers/users.go --with web-b
$H render -f .hydra/process-compose.yaml --slot 1 --changed apps/api/handlers/users.go --with web-b -o .hydra/slot1.yaml
process-compose up -D -U -u "$sock_wt" -f .hydra/slot1.yaml -L .hydra/slot1.log

for _ in $(seq 50); do
  curl -sf localhost:20101 >/dev/null 2>&1 && curl -sf localhost:20000 >/dev/null 2>&1 && break
  sleep 0.1
done

chain() { python3 chain.py; }

echo
echo "web-a main     : $(curl -s localhost:20000 | chain)"
echo "web-b main     : $(curl -s localhost:20001 | chain)"
echo "web-b worktree : $(curl -s localhost:20101 | chain)"
