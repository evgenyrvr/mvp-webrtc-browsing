#!/bin/sh
set -eu
project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
export GOPATH="$project_root/.cache/gopath"
export GOCACHE="$project_root/.cache/go-build"
# This workspace may expose a read-only placeholder .git directory.
export GOFLAGS="${GOFLAGS:-} -buildvcs=false"
if [ -x "$project_root/.tools/go/bin/go" ]; then
  exec "$project_root/.tools/go/bin/go" "$@"
fi
exec go "$@"
