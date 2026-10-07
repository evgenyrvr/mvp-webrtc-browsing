#!/bin/sh
set -eu
project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_root"
umask 077
mkdir -p testing/data bin
./tools/go.sh build -o bin/runner ./cmd/runner
if ! docker network inspect mvp1-testing >/dev/null 2>&1; then
  docker network create --internal mvp1-testing >/dev/null
fi
exec ./bin/runner -socket "$project_root/testing/data/runner.sock" -namespace mvp1-testing -network mvp1-testing -public-ip "${MVP_PUBLIC_IP:-127.0.0.1}" -udp-base 50000
