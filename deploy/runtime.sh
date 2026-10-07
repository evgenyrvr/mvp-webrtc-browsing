#!/bin/sh
set -eu
project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_root"
: "${MVP_PUBLIC_IP:?Set IP advertised to WebRTC clients}"
: "${MVP_APP_NETWORK:?Set an existing Docker network with approved egress rules}"
umask 077
mkdir -p deploy/data bin
./tools/go.sh build -o bin/runner ./cmd/runner
exec ./bin/runner -socket "$project_root/deploy/data/runner.sock" -namespace mvp1-deploy -network "$MVP_APP_NETWORK" -public-ip "$MVP_PUBLIC_IP" -udp-base 51000
