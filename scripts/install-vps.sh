#!/usr/bin/env bash
# Install bansos-warp-proxy on a VPS (Debian/Ubuntu).
# Run as root, from the cloned repo directory.
# Usage (do NOT source it with ". " — run it as a command):
#   sudo bash ./scripts/install-vps.sh
#   # or, after chmod +x:
#   sudo ./scripts/install-vps.sh
# It does NOT set your allowed_source_ips or token: edit
# /opt/bansos-warp-proxy/config.json and /etc/bansos-warp-proxy.env
# after install, then restart the service.
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root: sudo $0" >&2
  exit 1
fi

# Resolve the repo root from the script's own location, not from $0.
# $0 is the shell itself when this file is sourced (". ./scripts/..."),
# which is why the old version cd'd to the wrong directory and then
# failed with "go.mod file not found". BASH_SOURCE[0] is the script
# path even when sourced, so this works in both cases.
SCRIPT_PATH="${BASH_SOURCE[0]:-$0}"
SCRIPT_DIR="$(cd "$(dirname "$SCRIPT_PATH")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# Fallback: if the resolved dir has no go.mod but the current
# directory does, the user is already in the repo root — use that.
if [ ! -f "$REPO_DIR/go.mod" ] && [ -f "$PWD/go.mod" ]; then
  REPO_DIR="$PWD"
fi
if [ ! -f "$REPO_DIR/go.mod" ]; then
  echo "ERROR: go.mod not found in $REPO_DIR" >&2
  echo "Run this script from the repo root, e.g.:" >&2
  echo "  cd /home/ubuntu/bansos-warp-proxy && sudo bash ./scripts/install-vps.sh" >&2
  exit 1
fi
INSTALL_DIR=/opt/bansos-warp-proxy

echo "== install Go (if missing or older than 1.24) =="
GO_OK=0
if command -v go >/dev/null 2>&1; then
  # go version prints e.g. "go version go1.24.0 linux/amd64"
  GO_VER="$(go version 2>/dev/null | awk '{print $3}' | sed 's/^go//')"
  GO_MAJOR="$(echo "$GO_VER" | cut -d. -f1)"
  GO_MINOR="$(echo "$GO_VER" | cut -d. -f2)"
  if [ "$GO_MAJOR" -gt 1 ] || { [ "$GO_MAJOR" -eq 1 ] && [ "$GO_MINOR" -ge 24 ]; }; then
    GO_OK=1
  fi
fi
if [ "$GO_OK" -eq 0 ]; then
  ARCH="$(uname -m)"
  case "$ARCH" in
    x86_64|amd64) GO_ARCH=amd64 ;;
    aarch64|arm64) GO_ARCH=arm64 ;;
    *) echo "ERROR: unsupported architecture $ARCH" >&2; exit 1 ;;
  esac
  apt-get update
  apt-get install -y wget ca-certificates
  GO_TARBALL="go1.24.0.linux-${GO_ARCH}.tar.gz"
  wget -q "https://go.dev/dl/${GO_TARBALL}" -O "/tmp/${GO_TARBALL}"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "/tmp/${GO_TARBALL}"
  rm "/tmp/${GO_TARBALL}"
  export PATH="/usr/local/go/bin:$PATH"
fi
export PATH="/usr/local/go/bin:$PATH"
go version

echo "== build (in $REPO_DIR) =="
cd "$REPO_DIR"
go mod tidy
go mod download
go vet ./...
go build -trimpath -o bansos-warp-proxy ./cmd/bansos-warp-proxy

echo "== install files =="
id -u bansoswarp >/dev/null 2>&1 || useradd -r -s /usr/sbin/nologin bansoswarp
mkdir -p "$INSTALL_DIR"
cp bansos-warp-proxy "$INSTALL_DIR/"
cp config.example.json "$INSTALL_DIR/config.example.json"
if [ ! -f "$INSTALL_DIR/config.json" ]; then
  cp config.example.json "$INSTALL_DIR/config.json"
  echo "EDIT $INSTALL_DIR/config.json: set allowed_source_ips to your bansos machine's public IP"
fi
chown -R bansoswarp:bansoswarp "$INSTALL_DIR"
chmod 750 "$INSTALL_DIR"
chmod 600 "$INSTALL_DIR/config.json" || true

if [ ! -f /etc/bansos-warp-proxy.env ]; then
  printf '# Optional: CONNECT token (relay does not use it, see README).\n# BANSOS_WARP_TOKEN=\n' > /etc/bansos-warp-proxy.env
  chmod 600 /etc/bansos-warp-proxy.env
fi

cp systemd/bansos-warp-proxy.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable bansos-warp-proxy
echo "== done =="
echo "1. Edit $INSTALL_DIR/config.json (allowed_source_ips, listen)"
echo "2. Firewall the port to that IP only, e.g.:"
echo "     ufw allow from <YOUR_IP> to any port 18080 proto tcp"
echo "     ufw deny 18080/tcp   # if ufw default is not already deny"
echo "3. systemctl restart bansos-warp-proxy && journalctl -u bansos-warp-proxy -f"
echo "4. From your laptop (not the VPS): curl http://<VPS_IP>:18080/healthz"
