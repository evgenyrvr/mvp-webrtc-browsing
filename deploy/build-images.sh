#!/bin/sh
set -eu
project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_root"
registry_prefix=${1:?Usage: ./deploy/build-images.sh REGISTRY/PROJECT VERSION}
version=${2:?Specify a release version; avoid mutable latest}
mkdir -p bin
CGO_ENABLED=0 ./tools/go.sh build -trimpath -o bin/session-worker ./cmd/session-worker
CGO_ENABLED=0 ./tools/go.sh build -trimpath -o bin/gateway ./cmd/gateway
CGO_ENABLED=0 ./tools/go.sh build -trimpath -o bin/runner ./cmd/runner
docker build -t "$registry_prefix/mvp1-worker:$version" -f deploy/worker/Dockerfile .
docker build -t "$registry_prefix/mvp1-control:$version" -f deploy/control/Dockerfile .
printf 'Built %s/mvp1-worker:%s and %s/mvp1-control:%s. Push after verification.\n' "$registry_prefix" "$version" "$registry_prefix" "$version"
