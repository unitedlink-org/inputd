#!/usr/bin/env bash
# deploy/install.sh — install or upgrade inputd on Ubuntu 24.04
# Must be run as root. Idempotent: safe to run multiple times.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BINARY=/usr/local/bin/inputd
MIN_GO_MINOR=22

# ── prerequisites ─────────────────────────────────────────────────────────
if ! command -v go &>/dev/null; then
  echo "ERROR: go not found — install Go $MIN_GO_MINOR or newer"
  exit 1
fi
GO_MINOR=$(go version | grep -oP 'go1\.\K[0-9]+')
if [ "${GO_MINOR:-0}" -lt "$MIN_GO_MINOR" ]; then
  echo "ERROR: Go 1.$MIN_GO_MINOR+ required, found $(go version)"
  exit 1
fi

# ── build ──────────────────────────────────────────────────────────────────
echo "==> Building inputd..."
cd "$REPO_DIR"
go build -o "$BINARY" ./cmd/inputd
echo "    installed: $BINARY ($(go version | awk '{print $3}'))"

# ── system group ──────────────────────────────────────────────────────────
if ! getent group ops > /dev/null 2>&1; then
  groupadd --system ops
  echo "==> Created group: ops"
fi

# ── udev rules ────────────────────────────────────────────────────────────
echo "==> Installing udev rules..."
cp "$REPO_DIR/deploy/99-primary_keypad.rules"     /etc/udev/rules.d/
cp "$REPO_DIR/deploy/99-secondary_keypad.rules" /etc/udev/rules.d/
udevadm control --reload-rules
udevadm trigger --subsystem-match=input
echo "    /etc/udev/rules.d/99-primary_keypad.rules"
echo "    /etc/udev/rules.d/99-secondary_keypad.rules"

# ── journald limits ───────────────────────────────────────────────────────
echo "==> Configuring journald limits..."
mkdir -p /etc/systemd/journald.conf.d
cp "$REPO_DIR/deploy/journald.conf.d/limits.conf" \
   /etc/systemd/journald.conf.d/inputd-limits.conf
systemctl kill --signal=SIGUSR2 systemd-journald 2>/dev/null || true
echo "    /etc/systemd/journald.conf.d/inputd-limits.conf"

# ── config (first install only; never overwrite existing bindings) ────────
if [ ! -f /etc/inputd/config.yaml ]; then
  echo "==> Installing default config..."
  mkdir -p /etc/inputd
  cp "$REPO_DIR/deploy/config.yaml" /etc/inputd/config.yaml
  echo "    /etc/inputd/config.yaml"
else
  echo "==> Config already exists, skipping (edit manually or use Web UI)"
fi

# ── systemd service ───────────────────────────────────────────────────────
echo "==> Installing systemd service..."
cp "$REPO_DIR/deploy/inputd.service" /etc/systemd/system/inputd.service
systemctl daemon-reload
systemctl enable inputd

if systemctl is-active --quiet inputd; then
  systemctl restart inputd
  echo "==> inputd restarted"
else
  systemctl start inputd
  echo "==> inputd started"
fi

# ── verify ────────────────────────────────────────────────────────────────
sleep 1
if systemctl is-active --quiet inputd; then
  echo "==> inputd is running"
  curl -sf --unix-socket /run/inputd/control.sock http://localhost/v1/status \
    | python3 -m json.tool 2>/dev/null || true
else
  echo "ERROR: inputd failed to start"
  journalctl -u inputd -n 20 --no-pager
  exit 1
fi
