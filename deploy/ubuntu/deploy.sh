#!/usr/bin/env bash
# Ubuntu VPS installer. Run as root; never run with shell tracing (bash -x).
set -Eeuo pipefail
umask 077

usage() {
  cat <<'HELP'
Usage:
  sudo bash deploy.sh install PUBLIC_IPV4 [DNS_NAME]
  sudo bash deploy.sh start|stop|restart|status|logs

Single Ubuntu 22.04+ VPS with its public IPv4 on a local interface.
HTTPS: 8443/tcp; TURN: 3478/tcp+udp; media: 51000-51003/udp;
TURN relay: 52000-52100/udp. Open these ports in your provider firewall.
No firewall or SSH rules are changed by this installer.

Optional install environment:
  MVP_REVISION   Git commit/tag to build (default: tested pilot commit)
  MVP_DOCKER_DNS Optional comma-separated DNS server IPs for MVP containers
  MVP_SKIP_PACKAGES=1  Reuse already installed dependencies (checked)
  MVP_DEBIAN_MIRROR    Optional Debian mirror hostname (both repositories)
  TLS_CERT      Existing PEM fullchain (requires TLS_KEY)
  TLS_KEY       Existing PEM private key
Without TLS_CERT/TLS_KEY a 365-day self-signed certificate is created.
Existing database, credentials and certificate are preserved on reinstall.
HELP
}
[[ "${1:-}" != --help && "${1:-}" != -h ]] || { usage; exit 0; }
[[ $EUID == 0 ]] || { echo 'Run with sudo or as root.' >&2; exit 1; }
action=${1:-help}
case "$action" in
  start|stop|restart) systemctl "$action" mvp-webrtc.target; exit ;;
  status) systemctl --no-pager status mvp-webrtc-{gateway,runner,turn}.service; exit ;;
  logs) journalctl -u mvp-webrtc-gateway -u mvp-webrtc-runner -u mvp-webrtc-turn -n 100 -f; exit ;;
  install) ;;
  *) usage; exit 1 ;;
esac
[[ $# -ge 2 && $# -le 3 ]] || { usage; exit 1; }
public_ip=$2
public_host=${3:-$public_ip}
[[ $public_ip =~ ^[0-9.]+$ && $public_host =~ ^[a-zA-Z0-9][a-zA-Z0-9.-]*$ ]] || { echo 'Invalid address/hostname.' >&2; exit 1; }
source /etc/os-release
[[ $ID == ubuntu && ${VERSION_ID%%.*} -ge 22 ]] || { echo 'Ubuntu 22.04 or newer required.' >&2; exit 1; }
service_user=mvp-webrtc
root_dir=/opt/mvp-webrtc
state_dir=/var/lib/mvp-webrtc
config_dir=/etc/mvp-webrtc
revision=${MVP_REVISION:-6a5b0ddc95d29ce92ca2ed8966250ef15a290bb0}
[[ $revision =~ ^[a-zA-Z0-9._/-]+$ && $revision != -* ]] || { echo 'Invalid revision.' >&2; exit 1; }
[[ -z ${TLS_CERT:-} && -z ${TLS_KEY:-} || -f ${TLS_CERT:-/nonexistent} && -f ${TLS_KEY:-/nonexistent} ]] || { echo 'Provide both TLS_CERT and TLS_KEY.' >&2; exit 1; }
trap 'echo "Deployment failed at line $LINENO. Check: sudo journalctl -u mvp-webrtc-gateway -u mvp-webrtc-runner -u mvp-webrtc-turn -n 80" >&2' ERR

# Do not take over an unrelated account, directory or coturn installation.
if id "$service_user" >/dev/null 2>&1 && [[ ! -f "$config_dir/managed" ]]; then
  echo "Account $service_user already exists and is not managed by this installer." >&2; exit 1
fi
if [[ -e $root_dir || -e $config_dir || -e $state_dir ]]; then
  [[ -f "$config_dir/managed" ]] || { echo 'Deployment paths already exist; refusing to overwrite.' >&2; exit 1; }
fi
reinstall=0
[[ ! -f "$config_dir/managed" ]] || reinstall=1
fresh_turn=1
if dpkg-query -W -f='${Status}' coturn 2>/dev/null | grep -q 'install ok installed'; then
  fresh_turn=0
  if [[ ! -f "$config_dir/managed" ]]; then echo 'coturn is already installed; use a clean VPS or configure TURN manually.' >&2; exit 1; fi
fi
export DEBIAN_FRONTEND=noninteractive
# Defer unrelated service restarts; wait for Ubuntu automatic updates.
export NEEDRESTART_MODE=l
if [[ ${MVP_SKIP_PACKAGES:-0} == 1 ]]; then
  for package in ca-certificates git python3 openssl curl iproute2 coturn; do
    [[ $(dpkg-query -W -f='${Status}' "$package" 2>/dev/null) == 'install ok installed' ]] || { echo "Missing package: $package" >&2; exit 1; }
  done
  command -v docker >/dev/null || { echo 'Docker is not installed.' >&2; exit 1; }
else
  apt-get update
  apt-get -o DPkg::Lock::Timeout=300 install -y ca-certificates git python3 openssl curl iproute2 coturn
  if ! command -v docker >/dev/null; then apt-get -o DPkg::Lock::Timeout=300 install -y docker.io; fi
fi
browser_cpus=$(python3 -c 'import os; print(min(2.5, os.cpu_count() or 1))')
systemctl enable --now docker
if (( fresh_turn )); then systemctl disable --now coturn; fi
if (( ! reinstall )); then
  if ss -H -lntu | awk '{print $5}' | grep -Eq ':(8443|3478)$'; then
    echo 'Port 8443 or 3478 is already in use; refusing to replace another service.' >&2; exit 1
  fi
fi
python3 - "$public_ip" "$public_host" <<'PY'
import ipaddress,json,subprocess,sys
ip=ipaddress.IPv4Address(sys.argv[1])
if not ip.is_global: raise SystemExit("A globally routable public IPv4 is required.")
addresses=json.loads(subprocess.check_output(['ip','-j','-4','addr','show']))
if not any(a['local']==str(ip) for link in addresses for a in link['addr_info']):
    raise SystemExit("Public IP is not on a local interface. This installer requires a direct-address VPS (not NAT).")
host=sys.argv[2]
if len(host)>253 or any(not part or len(part)>63 or part.startswith('-') or part.endswith('-') for part in host.split('.')):
    raise SystemExit("Invalid hostname")
PY
if [[ -f "$config_dir/address" && $(cat "$config_dir/address") != "$public_ip $public_host" ]]; then
  echo 'Address differs from the installed configuration. Migrate address/certificate explicitly.' >&2; exit 1
fi
install -d -m 700 "$config_dir"
touch "$config_dir/managed"
if ! id "$service_user" >/dev/null 2>&1; then
  useradd --system --user-group --create-home --home-dir "$state_dir" --shell /usr/sbin/nologin "$service_user"
fi
usermod -aG docker "$service_user"
install -d -m 700 -o "$service_user" -g "$service_user" "$state_dir"
install -d -m 755 "$root_dir"
printf '%s %s\n' "$public_ip" "$public_host" > "$config_dir/address"

# Build a fresh checkout; never copy a local test DB, profiles or credentials.
release=$(mktemp -d "$root_dir/release-XXXXXXXX")
git clone --quiet https://github.com/evgenyrvr/mvp-webrtc-browsing.git "$release/src"
git -C "$release/src" checkout --quiet --detach "$revision"
commit=$(git -C "$release/src" rev-parse HEAD)
chown -R "$service_user:$service_user" "$release"
go_version=$(awk '$1=="go"{print $2;exit}' "$release/src/go.mod")
# Download exactly the module's Go version, verified against official SHA-256 metadata.
python3 - "$go_version" "$release" <<'PY'
import hashlib,json,pathlib,platform,sys,urllib.request
version='go'+sys.argv[1]
arch={'x86_64':'amd64','aarch64':'arm64'}.get(platform.machine())
if not arch: raise SystemExit("Only amd64 and arm64 are supported.")
with urllib.request.urlopen('https://go.dev/dl/?mode=json&include=all',timeout=60) as r: releases=json.load(r)
item=next((f for v in releases if v['version']==version for f in v['files'] if f['os']=='linux' and f['arch']==arch and f['kind']=='archive'),None)
if not item: raise SystemExit("Requested Go version is not available from go.dev: "+version)
path=pathlib.Path(sys.argv[2])/'go.tar.gz'
digest=hashlib.sha256()
with urllib.request.urlopen('https://go.dev/dl/'+item['filename'],timeout=120) as r, path.open('wb') as out:
    while chunk:=r.read(1024*1024): digest.update(chunk); out.write(chunk)
if digest.hexdigest()!=item['sha256']: path.unlink(); raise SystemExit("Go checksum mismatch")
PY
tar -xzf "$release/go.tar.gz" -C "$release"
rm "$release/go.tar.gz"
runuser -u "$service_user" -- env PATH="$release/go/bin:/usr/bin:/bin" HOME="$state_dir" GOMAXPROCS=2 GOFLAGS=-p=2 CGO_ENABLED=0 bash -c '
  set -e
  cd "$1"
  mkdir -p bin
  for name in gateway runner session-worker; do
    ./tools/go.sh build -trimpath -o "bin/$name" "./cmd/$name"
  done
' _ "$release/src"
image="mvp1-worker:$commit"
worker_dockerfile="$release/src/deploy/worker/Dockerfile"
if [[ -n ${MVP_DEBIAN_MIRROR:-} ]]; then
  [[ $MVP_DEBIAN_MIRROR =~ ^[a-zA-Z0-9][a-zA-Z0-9.-]+$ ]] || { echo 'Invalid Debian mirror hostname.' >&2; exit 1; }
  worker_dockerfile="$release/worker.Dockerfile"
  sed "s|^RUN apt-get update|RUN sed -i s,deb.debian.org,$MVP_DEBIAN_MIRROR,g /etc/apt/sources.list.d/debian.sources \&\& apt-get update|" "$release/src/deploy/worker/Dockerfile" > "$worker_dockerfile"
fi
docker build -t "$image" -f "$worker_dockerfile" "$release/src"
if ! docker network inspect mvp-webrtc >/dev/null 2>&1; then
  docker network create --label mvp1.managed=ubuntu-deploy mvp-webrtc >/dev/null
fi
[[ $(docker network inspect mvp-webrtc --format '{{.Internal}}') == false ]] || { echo 'mvp-webrtc must be a regular bridge network.' >&2; exit 1; }

install -d -m 750 -o root -g "$service_user" "$config_dir/tls"
if [[ ! -f "$config_dir/tls/cert.pem" ]]; then
  if [[ -n ${TLS_CERT:-} ]]; then
    install -m 644 "$TLS_CERT" "$config_dir/tls/cert.pem"
    install -m 640 "$TLS_KEY" "$config_dir/tls/key.pem"
  else
    openssl req -x509 -newkey rsa:2048 -nodes -days 365       -subj "/CN=$public_host" -addext "subjectAltName=IP:$public_ip,DNS:$public_host"       -keyout "$config_dir/tls/key.pem" -out "$config_dir/tls/cert.pem" >/dev/null 2>&1
  fi
fi
chown root:"$service_user" "$config_dir/tls/"*.pem
chmod 640 "$config_dir/tls/key.pem" "$config_dir/tls/cert.pem"
# Allow traversal to TLS for the service, while secret files remain root-only.
chmod 751 "$config_dir"
admin_password=''
if [[ ! -f "$config_dir/service.env" ]]; then
  [[ ! -f "$state_dir/mvp1.db" ]] || { echo 'Database exists but service.env is missing; restore configuration first.' >&2; exit 1; }
  turn_secret=$(openssl rand -hex 32)
  admin_password=$(openssl rand -hex 16)
  printf 'MVP_TURN_URL=turn:%s:3478?transport=udp\nMVP_TURN_SECRET=%s\nMVP_ADMIN_LOGIN=admin\nMVP_ADMIN_PASSWORD=%s\n' "$public_ip" "$turn_secret" "$admin_password" > "$config_dir/service.env"
fi
if [[ -n ${MVP_DOCKER_DNS:-} ]]; then
  python3 - "$MVP_DOCKER_DNS" <<'PYDNS'
import ipaddress,sys
for address in sys.argv[1].split(','): ipaddress.ip_address(address)
PYDNS
  sed -i '/^MVP_DOCKER_DNS=/d' "$config_dir/service.env"
  printf 'MVP_DOCKER_DNS=%s\n' "$MVP_DOCKER_DNS" >> "$config_dir/service.env"
fi
admin_password=$(sed -n 's/^MVP_ADMIN_PASSWORD=//p' "$config_dir/service.env")
turn_secret=$(sed -n 's/^MVP_TURN_SECRET=//p' "$config_dir/service.env")
[[ $turn_secret =~ ^[a-f0-9]{64}$ ]] || { echo 'Invalid saved TURN secret.' >&2; exit 1; }
cat > "$config_dir/turn.conf" <<TURN
listening-port=3478
listening-ip=$public_ip
relay-ip=$public_ip
min-port=52000
max-port=52100
realm=$public_host
fingerprint
use-auth-secret
static-auth-secret=$turn_secret
no-cli
no-tls
no-dtls
no-multicast-peers
denied-peer-ip=0.0.0.0-255.255.255.255
denied-peer-ip=::-ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff
allowed-peer-ip=$public_ip
user-quota=12
total-quota=48
log-file=stdout
simple-log
TURN
chown root:turnserver "$config_dir/turn.conf"
chmod 640 "$config_dir/turn.conf"
# Stop old services only after build succeeds.
systemctl stop mvp-webrtc.target 2>/dev/null || true
ln -sfn "$release/src" "$root_dir/current"
for component in turn runner gateway; do
  case "$component" in
    turn)
      svc_user=turnserver
      dependencies='network-online.target'
      command="/usr/bin/turnserver -c $config_dir/turn.conf --pidfile=/run/mvp-webrtc-turn/turn.pid"
      extra='RuntimeDirectory=mvp-webrtc-turn'
      ;;
    runner)
      svc_user=$service_user
      dependencies='docker.service mvp-webrtc-turn.service'
      command="$root_dir/current/bin/runner -socket $state_dir/runner.sock -namespace mvp1-deploy -network mvp-webrtc -public-ip $public_ip -udp-base 51000 -browser-cpus $browser_cpus -image $image"
      extra="EnvironmentFile=$config_dir/service.env"
      ;;
    gateway)
      svc_user=$service_user
      dependencies='mvp-webrtc-runner.service'
      command="$root_dir/current/bin/gateway -db $state_dir/mvp1.db -addr 0.0.0.0:8443 -origin https://$public_host:8443 -tls-cert $config_dir/tls/cert.pem -tls-key $config_dir/tls/key.pem -runner-socket $state_dir/runner.sock"
      extra="EnvironmentFile=$config_dir/service.env"
      ;;
  esac
  cat > "/etc/systemd/system/mvp-webrtc-$component.service" <<UNIT
[Unit]
Description=MVP WebRTC $component
After=$dependencies
Requires=$dependencies
PartOf=mvp-webrtc.target

[Service]
Type=simple
User=$svc_user
Group=$svc_user
$extra
WorkingDirectory=$state_dir
ExecStart=$command
Restart=on-failure
RestartSec=3
TimeoutStopSec=45
UMask=0077
NoNewPrivileges=true

[Install]
WantedBy=mvp-webrtc.target
UNIT
done
# turnserver must not use the application's private working directory.
sed -i 's|^WorkingDirectory=.*|WorkingDirectory=/|' /etc/systemd/system/mvp-webrtc-turn.service
cat > /etc/systemd/system/mvp-webrtc.target <<'UNIT'
[Unit]
Description=MVP WebRTC application
Wants=mvp-webrtc-turn.service mvp-webrtc-runner.service mvp-webrtc-gateway.service
After=network-online.target

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now mvp-webrtc.target
ready=0
for ((i=0;i<60;i++)); do
  if curl --silent --fail --insecure --max-time 2 https://127.0.0.1:8443/readyz >/dev/null; then ready=1; break; fi
  sleep 1
done
(( ready )) || { echo 'Gateway readiness failed.' >&2; exit 1; }
for component in turn runner gateway; do systemctl is-active --quiet "mvp-webrtc-$component"; done
# Bootstrap password is only needed once; SQLite contains the Argon2id hash.
sed -i '/^MVP_ADMIN_PASSWORD=/d' "$config_dir/service.env"
printf '\nReady: https://%s:8443\nCommit: %s\nUser: admin\n' "$public_host" "$commit"
if [[ -n $admin_password ]]; then printf 'Initial password (save now): %s\n' "$admin_password"; fi
printf 'Open firewall ports: TCP 8443,3478; UDP 3478,51000-51003,52000-52100.\n'
printf 'HTTP readiness passed. Verify remote WebRTC, input and printing from another computer.\n'
