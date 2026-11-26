#!/bin/sh
# Install BlobFish on Linux (amd64 or arm64).
#
#   curl -fsSL https://raw.githubusercontent.com/nitishkdas/blobfish/main/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --systemd   # also set up a service
#
# Downloads a release from GitHub, checks it against the release's
# SHA256SUMS, and installs the binary to /usr/local/bin. With --systemd it
# also creates a "blobfish" system user, /var/lib/blobfish,
# /etc/blobfish/config.json (listening on 127.0.0.1:8080 only) and a
# hardened systemd unit, then starts the service. It never overwrites an
# existing config or data.
#
# Options (or environment variables):
#   --version vX.Y.Z   BLOBFISH_VERSION   release to install (default: latest)
#   --systemd                             set up and start the service
#   --prefix DIR       BLOBFISH_PREFIX    install dir (default /usr/local/bin)
#                      BLOBFISH_RELEASES_URL  a mirror laid out like GitHub's
#                                             .../releases (latest, download/)
set -eu

releases="${BLOBFISH_RELEASES_URL:-https://github.com/nitishkdas/blobfish/releases}"
version="${BLOBFISH_VERSION:-}"
prefix="${BLOBFISH_PREFIX:-/usr/local/bin}"
systemd=0

while [ $# -gt 0 ]; do
	case "$1" in
	--version) version="$2"; shift 2 ;;
	--systemd) systemd=1; shift ;;
	--prefix) prefix="$2"; shift 2 ;;
	-h | --help) sed -n '2,20p' "$0" 2>/dev/null || true; exit 0 ;;
	*) echo "install.sh: unknown option $1" >&2; exit 2 ;;
	esac
done

say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

[ "$(uname -s)" = Linux ] || die "BlobFish releases are for Linux; build from source elsewhere"
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "no release for $(uname -m); build from source (see README)" ;;
esac
command -v curl >/dev/null || die "curl is needed"
command -v sha256sum >/dev/null || die "sha256sum is needed"

sudo=""
if [ "$(id -u)" -ne 0 ]; then
	command -v sudo >/dev/null || die "run as root, or install sudo"
	sudo="sudo"
fi

if [ -z "$version" ]; then
	# The "latest" page redirects to .../releases/tag/vX.Y.Z.
	url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$releases/latest")
	version="${url##*/}"
	case "$version" in v*) ;; *) die "couldn't find the latest release (got $url)" ;; esac
fi

name="blobfish_${version#v}_linux_${arch}"
base="$releases/download/$version"
tmp=$(mktemp -d)
trap 'rm -r "$tmp"' EXIT

say "Downloading BlobFish $version for linux/$arch..."
curl -fsSL -o "$tmp/$name.tar.gz" "$base/$name.tar.gz"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS"
(cd "$tmp" && grep " $name.tar.gz\$" SHA256SUMS | sha256sum -c --status) ||
	die "checksum mismatch for $name.tar.gz: not installing"
tar -xzf "$tmp/$name.tar.gz" -C "$tmp"

$sudo install -d "$prefix"
$sudo install -m 0755 "$tmp/$name/blobfish" "$prefix/blobfish"
say "Installed $prefix/blobfish ($("$prefix/blobfish" -version))"

if [ "$systemd" -eq 0 ]; then
	say "Run it with:  $prefix/blobfish   (data in ./data, http://127.0.0.1:8080/)"
	say "For a server, see docs/deploy.md, or re-run with --systemd."
	exit 0
fi

command -v systemctl >/dev/null || die "--systemd needs systemd"
if ! id blobfish >/dev/null 2>&1; then
	$sudo useradd --system --home /var/lib/blobfish --shell /usr/sbin/nologin blobfish
fi
$sudo install -d -m 0750 -o blobfish -g blobfish /var/lib/blobfish /etc/blobfish

if [ ! -e /etc/blobfish/config.json ]; then
	cat >"$tmp/config.json" <<'JSON'
{
  "listen": "127.0.0.1:8080",
  "data_dir": "/var/lib/blobfish",
  "trusted_proxies": ["127.0.0.1", "::1"],
  "log_format": "json"
}
JSON
	$sudo install -m 0640 -o root -g blobfish "$tmp/config.json" /etc/blobfish/config.json
	say "Wrote /etc/blobfish/config.json (set public_url once you have a domain)"
fi

if [ ! -e /etc/systemd/system/blobfish.service ]; then
	cat >"$tmp/blobfish.service" <<UNIT
[Unit]
Description=BlobFish object store
After=network.target

[Service]
User=blobfish
Group=blobfish
ExecStart=$prefix/blobfish -config /etc/blobfish/config.json
Restart=on-failure
TimeoutStopSec=30
LimitNOFILE=65536
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ReadWritePaths=/var/lib/blobfish
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged @resources
CapabilityBoundingSet=
AmbientCapabilities=
RestrictSUIDSGID=yes
RestrictRealtime=yes
ProtectClock=yes
ProtectHostname=yes
ProtectKernelLogs=yes
ProtectProc=invisible
ProcSubset=pid
PrivateUsers=yes
RemoveIPC=yes
UMask=0077

[Install]
WantedBy=multi-user.target
UNIT
	$sudo install -m 0644 "$tmp/blobfish.service" /etc/systemd/system/blobfish.service
fi

$sudo systemctl daemon-reload
$sudo systemctl enable --now blobfish
# An upgrade: the running service still has the old binary.
$sudo systemctl restart blobfish

say ""
say "BlobFish is running on 127.0.0.1:8080 (logs: journalctl -u blobfish)."
if $sudo test -e /var/lib/blobfish/initial-admin-key; then
	say "Your first admin key:   sudo cat /var/lib/blobfish/initial-admin-key"
	say "Store it safely, then:  sudo rm /var/lib/blobfish/initial-admin-key"
fi
say "Next: put Caddy in front for HTTPS (docs/deploy.md, step 4)."
