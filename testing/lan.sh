#!/bin/sh
# Run the test database on the LAN over HTTPS, with host-published media ports.
set -eu
project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_root"
public_ip=${1:?Usage: ./testing/lan.sh SERVER_LAN_IPV4}
python3 - "$public_ip" <<'PY'
import ipaddress, sys
ip = ipaddress.IPv4Address(sys.argv[1])
if ip.is_loopback or ip.is_unspecified or ip.is_multicast:
    raise SystemExit('Specify the LAN IPv4 address of this server')
PY
umask 077
mkdir -p testing/data bin
cert_dir="$project_root/testing/data/tls-$public_ip"
mkdir -p "$cert_dir"
if [ ! -s "$cert_dir/cert.pem" ] || [ ! -s "$cert_dir/key.pem" ]; then
  openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
    -keyout "$cert_dir/key.pem" -out "$cert_dir/cert.pem" \
    -subj "/CN=$public_ip" -addext "subjectAltName=IP:$public_ip" >/dev/null 2>&1
fi
./tools/go.sh build -o bin/runner ./cmd/runner
./tools/go.sh build -o bin/gateway ./cmd/gateway
docker image inspect mvp1-worker:pilot >/dev/null
if ! docker network inspect mvp1-testing-lan >/dev/null 2>&1; then
  docker network create mvp1-testing-lan >/dev/null
fi
# Refuse accidentally reused internal networks rather than silently losing port forwarding.
if [ "$(docker network inspect mvp1-testing-lan --format '{{.Internal}}')" != false ]; then
  echo 'mvp1-testing-lan must be a regular bridge network' >&2
  exit 1
fi
socket="$project_root/testing/data/lan-runner.sock"
if [ -e "$socket" ]; then
  echo "Runner socket already exists: $socket. Stop the previous LAN runner first." >&2
  exit 1
fi
runner_pid=
gateway_pid=
cleanup() {
  trap - EXIT INT TERM
  if [ -n "$gateway_pid" ]; then kill -TERM "$gateway_pid" 2>/dev/null || true; wait "$gateway_pid" 2>/dev/null || true; fi
  if [ -n "$runner_pid" ]; then kill -TERM "$runner_pid" 2>/dev/null || true; wait "$runner_pid" 2>/dev/null || true; fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
./bin/runner -socket "$socket" -namespace mvp1-testing-lan -network mvp1-testing-lan -public-ip "$public_ip" -udp-base 50300 -browser-cpus "${MVP_BROWSER_CPUS:-2.5}" &
runner_pid=$!
count=0
while [ ! -S "$socket" ]; do
  kill -0 "$runner_pid" 2>/dev/null || { echo 'Runner failed to start' >&2; exit 1; }
  count=$((count + 1))
  [ "$count" -lt 100 ] || { echo 'Runner startup timeout' >&2; exit 1; }
  sleep 0.1
done
printf 'Open https://%s:8443 (temporary test certificate)\n' "$public_ip"
set -- ./bin/gateway -db "$project_root/testing/data/mvp1.db" -addr 0.0.0.0:8443 \
  -origin "https://$public_ip:8443" -tls-cert "$cert_dir/cert.pem" -tls-key "$cert_dir/key.pem" -runner-socket "$socket"
if [ "${MVP_RELAY_ONLY:-0}" = 1 ]; then set -- "$@" -relay-only; fi
"$@" &
gateway_pid=$!
wait "$gateway_pid"
