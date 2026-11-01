#!/usr/bin/env bash
# Install bansos-warp-proxy on a VPS (Debian/Ubuntu).
# Run as root, from the cloned repo directory.
# Usage: sudo ./scripts/install-vps.sh
# It does NOT set your allowed_source_ips or token: edit
# /opt/bansos-warp-proxy/config.json and /etc/bansos-warp-proxy.env
# after install, then restart the service.
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root: sudo $0" >&2
  exit 1
fi

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
INSTALL_DIR=/opt/bansos-warp-proxy

echo "== install Go (if missing) =="
if ! command -v go >/dev/null 2>&1; then
  apt-get update
  apt-get install -y wget ca-certificates
  # Go 1.24, linux/amd64. Adjust GO_TARBALL for arm64 if needed.
  GO_TARBALL=go1.24.0.linux-amd64.tar.gz
  wget -q "https://go.dev/dl/${GO_TARBALL}" -O "/tmp/${GO_TARBALL}"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "/tmp/${GO_TARBALL}"
  rm "/tmp/${GO_TARBALL}"
  export PATH="/usr/local/go/bin:$PATH"
fi
export PATH="/usr/local/go/bin:$PATH"
go version

echo "== build =="
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
