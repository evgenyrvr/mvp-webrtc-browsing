#!/bin/sh
set -eu
project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_root"
MVP_DOCKER_TEST=1 ./tools/go.sh test -count=1 -timeout=150s -v ./internal/runner
