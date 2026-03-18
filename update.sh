#!/usr/bin/env bash
set -euo pipefail

echo "==> Pulling latest..."
git pull

echo "==> Stopping services..."
sudo systemctl stop narrowcast-uplink || true
sudo systemctl stop narrowcast || true

echo "==> Building..."
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
