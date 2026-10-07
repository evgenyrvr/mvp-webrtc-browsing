#!/bin/sh
set -eu
project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_root"
umask 077
mkdir -p testing/data bin
./tools/go.sh build -o bin/gateway ./cmd/gateway
set -- ./bin/gateway -db "$project_root/testing/data/mvp1.db" -addr 127.0.0.1:8080 -origin http://127.0.0.1:8080
if [ -S "$project_root/testing/data/runner.sock" ]; then set -- "$@" -runner-socket "$project_root/testing/data/runner.sock"; fi
if [ "${MVP_RELAY_ONLY:-0}" = 1 ]; then set -- "$@" -relay-only; fi
exec "$@"
