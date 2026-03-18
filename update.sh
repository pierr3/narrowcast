#!/usr/bin/env bash
set -euo pipefail

# Detect what's installed
HAS_RELAY=$(systemctl list-unit-files narrowcast-relay.service &>/dev/null && echo 1 || echo 0)
HAS_SDR=$(systemctl list-unit-files narrowcast.service &>/dev/null && echo 1 || echo 0)

echo "==> Pulling latest..."
git pull

if [ "$HAS_SDR" = "1" ] || [ "$HAS_RELAY" = "0" -a "$HAS_SDR" = "0" ]; then
    # Default to SDR+uplink if nothing detected
    echo "==> Stopping services..."
    sudo systemctl stop narrowcast-uplink || true
    sudo systemctl stop narrowcast || true

    echo "==> Building narrowcast + uplink..."
    go build -o narrowcast ./cmd/narrowcast
    go build -o narrowcast-uplink ./cmd/uplink

    echo "==> Installing binaries..."
    sudo cp narrowcast /usr/local/bin/narrowcast
    sudo cp narrowcast-uplink /usr/local/bin/narrowcast-uplink

    echo "==> Starting services..."
    sudo systemctl start narrowcast
    sudo systemctl start narrowcast-uplink

    echo "==> Done."
    sudo systemctl status narrowcast --no-pager -l
    sudo systemctl status narrowcast-uplink --no-pager -l
else
    echo "==> Stopping relay..."
    sudo systemctl stop narrowcast-relay || true

    echo "==> Building relay..."
    go build -o narrowcast-relay ./cmd/relay

    echo "==> Installing binary..."
    sudo cp narrowcast-relay /usr/local/bin/narrowcast-relay

    echo "==> Starting relay..."
    sudo systemctl start narrowcast-relay

    echo "==> Done."
    sudo systemctl status narrowcast-relay --no-pager -l
fi
