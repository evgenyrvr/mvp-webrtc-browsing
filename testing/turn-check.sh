#!/bin/sh
# Temporary coturn; never starts the user's LAN gateway or uses its database.
set -eu
project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_root"
public_ip=${1:?Usage: ./testing/turn-check.sh SERVER_LAN_IPV4 [udp|tcp]}
transport=${2:-udp}
case "$transport" in udp|tcp) ;; *) echo 'Transport must be udp or tcp' >&2; exit 1;; esac
python3 - "$public_ip" <<'PY'
import ipaddress,sys
ip=ipaddress.IPv4Address(sys.argv[1])
if ip.is_unspecified or ip.is_loopback or ip.is_multicast: raise SystemExit('Use server LAN IPv4')
PY
umask 077
mkdir -p testing/data
turn_dir=$(mktemp -d "$project_root/testing/data/turn-check.XXXXXX")
turn_container=
cleanup() {
  result=$?
  trap - EXIT INT TERM
  if [ -n "$turn_container" ] && [ "$result" -ne 0 ]; then
    docker logs --tail 40 "$turn_container" 2>&1 | python3 -c 'import os,sys; print(sys.stdin.read().replace(os.environ.get("MVP_TURN_SECRET","redacted"),"[redacted]"))'
  fi
  if [ -n "$turn_container" ]; then docker rm -f "$turn_container" >/dev/null 2>&1 || true; fi
  rm -rf -- "$turn_dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
MVP_TURN_SECRET=$(python3 -c 'import secrets; print(secrets.token_hex(32))')
export MVP_TURN_SECRET
python3 - "$turn_dir/turnserver.conf" "$public_ip" <<'PY'
import os,sys
from pathlib import Path
Path(sys.argv[1]).write_text(f'''listening-ip={sys.argv[2]}
relay-ip={sys.argv[2]}
listening-port=3479
min-port=52150
max-port=52199
realm=mvp1-test.internal
fingerprint
use-auth-secret
static-auth-secret={os.environ['MVP_TURN_SECRET']}
no-cli
no-tls
no-dtls
no-multicast-peers
no-tcp-relay
relay-threads=2
denied-peer-ip=0.0.0.0-255.255.255.255
allowed-peer-ip={sys.argv[2]}
user-quota=8
total-quota=16
log-file=stdout
pidfile=/tmp/turn.pid
''')
PY
turn_container=$(docker run -d --label mvp1.test=turn --network host \
  --user "$(id -u):$(id -g)" --cap-drop=ALL --cap-add=NET_BIND_SERVICE --security-opt=no-new-privileges \
  --read-only --tmpfs /tmp:rw,nosuid,nodev,size=16m --memory=128m --pids-limit=64 \
  -v "$turn_dir/turnserver.conf:/etc/coturn/turnserver.conf:ro" \
  coturn/coturn:4.18.0-r0@sha256:bbefd3e1fdfdc0d58770fe01b581fd8b00d9f3a5580d00acb77cf719a6bc78e3 \
  -c /etc/coturn/turnserver.conf)
python3 - "$public_ip" <<'PY'
import socket,sys,time
for attempt in range(50):
 try:
  with socket.create_connection((sys.argv[1],3479),timeout=.5): break
 except OSError:
  if attempt==49: raise
  time.sleep(.1)
PY
export MVP_TURN_URL="turn:$public_ip:3479?transport=$transport"
export MVP_BROWSER_LAN_IP="$public_ip"
export MVP_RELAY_ONLY=1
export MVP_BROWSER_KIND="${MVP_BROWSER_KIND:-firefox}"
./testing/browser-check.sh
