#!/bin/sh
set -eu
project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_root"
mkdir -p bin
CGO_ENABLED=0 ./tools/go.sh build -o bin/session-worker ./cmd/session-worker
docker build -t mvp1-worker:pilot -f deploy/worker/Dockerfile .
