#!/bin/sh
set -eu
project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_root"
: "${MVP_PUBLIC_ORIGIN:?Set HTTPS origin, for example https://mvp.example.internal}"
: "${MVP_TLS_CERT:?Set TLS certificate path}"
: "${MVP_TLS_KEY:?Set TLS key path}"
umask 077
mkdir -p deploy/data bin
./tools/go.sh build -o bin/gateway ./cmd/gateway
set -- ./bin/gateway -db "$project_root/deploy/data/mvp1.db" -addr "${MVP_LISTEN:-0.0.0.0:8443}" -origin "$MVP_PUBLIC_ORIGIN" -tls-cert "$MVP_TLS_CERT" -tls-key "$MVP_TLS_KEY"
if [ -S "$project_root/deploy/data/runner.sock" ]; then
  set -- "$@" -runner-socket "$project_root/deploy/data/runner.sock"
fi
if [ "${MVP_RELAY_ONLY:-0}" = 1 ]; then set -- "$@" -relay-only; fi
exec "$@"
