#!/bin/sh
set -eu
project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_root"
MVP_RTC_TEST=1 CGO_ENABLED=1 ./tools/go.sh test -race -count=1 ./internal/worker
