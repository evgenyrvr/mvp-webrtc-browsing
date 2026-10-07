#!/bin/sh
set -eu
project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_root"
mkdir -p bin testing/data
./tools/go.sh build -o bin/gateway ./cmd/gateway
./tools/go.sh build -o bin/runner ./cmd/runner
MVP_BROWSER_TEST=1 ./tools/go.sh test -count=1 -timeout=270s -v ./testing/browser
