#!/usr/bin/env bash
# Lumen Bridge for Linux — bare-metal install script.
#
# Run as root on a Debian 12 / Ubuntu 22.04+ host (LXC, VM, Pi, NAS).
# Idempotent — re-running upgrades the binary in place.
#
#   curl -fsSL https://raw.githubusercontent.com/lorislabapp/lumen-bridge-linux/main/scripts/install.sh | bash
#
# Or with a specific version:
#
#   curl -fsSL https://raw.githubusercontent.com/lorislabapp/lumen-bridge-linux/main/scripts/install.sh | LB_VERSION=v0.3.0 bash
#
# After install, edit /etc/lumen-bridge/config.yaml, then:
#   sudo -u lumen-bridge /usr/local/bin/lumen-bridge pair --config /etc/lumen-bridge/config.yaml --code <6-digit>
#   systemctl enable --now lumen-bridge
set -euo pipefail

# Some minimal LXC images set TMPDIR to a path that doesn't exist
# yet (e.g. /tmp/user/0). Force a known-good location so mktemp works.
export TMPDIR=/tmp

VERSION="${LB_VERSION:-v0.3.0}"
ARCH="$(dpkg --print-architecture 2>/dev/null || uname -m)"
case "$ARCH" in
  amd64|x86_64)  ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

USER_NAME=lumen-bridge
BIN_PATH=/usr/local/bin/lumen-bridge
CONF_DIR=/etc/lumen-bridge
STATE_DIR=/var/lib/lumen-bridge
TARBALL="lumen-bridge-linux-${ARCH}.tar.gz"
BASE_URL="https://github.com/lorislabapp/lumen-bridge-linux/releases/download/${VERSION}"
URL="${BASE_URL}/${TARBALL}"

echo ">>> Lumen Bridge for Linux installer (${VERSION} / ${ARCH})"

# 1. Prereqs (curl + ca-certificates are usually preinstalled but minimal LXC
#    images skip them).
if ! command -v curl >/dev/null; then
  echo ">>> Installing curl + ca-certificates"
  apt-get update -qq
  apt-get install -y -qq --no-install-recommends curl ca-certificates
fi

# 2. System user.
if ! id "$USER_NAME" >/dev/null 2>&1; then
  echo ">>> Creating system user $USER_NAME"
  useradd --system --home-dir "$STATE_DIR" --shell /usr/sbin/nologin \
          --comment "Lumen Bridge daemon" "$USER_NAME"
fi

# 3. Directories.
install -d -m 0750 -o "$USER_NAME" -g "$USER_NAME" "$CONF_DIR" "$STATE_DIR"

# 4. Binary.
echo ">>> Downloading $URL"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
curl -fsSL -o "$TMP/$TARBALL" "$URL"
# Verify against the release's SHA256SUMS when the release publishes one
# (v0.3.0+). Older releases have no checksum file.
if curl -fsSL -o "$TMP/SHA256SUMS" "${BASE_URL}/SHA256SUMS" 2>/dev/null; then
  (cd "$TMP" && grep " ${TARBALL}\$" SHA256SUMS | sha256sum -c -) \
    || { echo "Checksum verification failed for $TARBALL" >&2; exit 1; }
  echo ">>> Checksum OK"
fi
tar -xzf "$TMP/$TARBALL" -C "$TMP"
install -m 0755 "$TMP/lumen-bridge-linux-${ARCH}" "$BIN_PATH"
echo ">>> Installed $($BIN_PATH version)"

# 5. config.yaml — only on first install. Subsequent runs preserve user edits.
if [ ! -f "$CONF_DIR/config.yaml" ]; then
  echo ">>> Writing default config.yaml (edit it before starting the service)"
  cat > "$CONF_DIR/config.yaml" <<'YAML'
mqtt:
  host: 192.168.1.50          # your MQTT broker (NOT the Frigate web host)
  port: 1883
  tls: false
  username: frigate
  password: change-me
  topic_prefix: frigate
  client_id: lumen-bridge-linux

relay:
  url: https://relay.lorislab.fr
  device_token_path: /var/lib/lumen-bridge/device-token.json

frigate:
  base_url: http://192.168.1.50:5000   # optional — per-event snapshot + clip upload
YAML
  chmod 0640 "$CONF_DIR/config.yaml"
  chown "$USER_NAME:$USER_NAME" "$CONF_DIR/config.yaml"
fi

# 6. systemd unit.
cat > /etc/systemd/system/lumen-bridge.service <<'UNIT'
[Unit]
Description=Lumen Bridge — forwards Frigate detection events to the Lumen apps
Documentation=https://github.com/lorislabapp/lumen-bridge-linux
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=lumen-bridge
Group=lumen-bridge
ExecStart=/usr/local/bin/lumen-bridge run --config /etc/lumen-bridge/config.yaml
StateDirectory=lumen-bridge
StateDirectoryMode=0700
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/lumen-bridge
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6
RestrictNamespaces=true
LockPersonality=true
MemoryDenyWriteExecute=true
SystemCallArchitectures=native
SystemCallFilter=@system-service
Restart=on-failure
RestartSec=10
StartLimitIntervalSec=120
StartLimitBurst=10

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
echo ">>> systemd unit installed (not yet enabled)"

# 7. Friendly final reminder.
cat <<'NEXT'

──────────────────────────────────────────────────────────────────────
Install complete. Next steps:

  1. Edit broker host + MQTT credentials (+ Frigate base_url):
       editor /etc/lumen-bridge/config.yaml

  2. Check config + broker reachability, then watch decoded events:
       sudo -u lumen-bridge /usr/local/bin/lumen-bridge doctor --config /etc/lumen-bridge/config.yaml
       sudo -u lumen-bridge /usr/local/bin/lumen-bridge run --config /etc/lumen-bridge/config.yaml --dry-run

  3. Pair with the Lumen app (Settings → Paired Bridges → Pair a Bridge
     shows a 6-digit code), then on this host:
       sudo -u lumen-bridge /usr/local/bin/lumen-bridge pair \
         --config /etc/lumen-bridge/config.yaml --code <6-digit>

  4. Start the daemon:
       systemctl enable --now lumen-bridge
       systemctl status lumen-bridge
       journalctl -fu lumen-bridge
──────────────────────────────────────────────────────────────────────
NEXT
