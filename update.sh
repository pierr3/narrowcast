#!/usr/bin/env bash
set -euo pipefail

# Detect what's installed. The relay can be the legacy single unit
# (narrowcast-relay) and/or any number of templated instances
# (narrowcast-relay@PORT). Collect every enabled relay unit so update
# rebuilds the shared binary and bounces all of them.
shopt -s nullglob
RELAY_UNITS=()
systemctl list-unit-files narrowcast-relay.service &>/dev/null && RELAY_UNITS+=("narrowcast-relay")
for link in /etc/systemd/system/multi-user.target.wants/narrowcast-relay@*.service; do
    RELAY_UNITS+=("$(basename "$link" .service)")
done

HAS_RELAY=$([ "${#RELAY_UNITS[@]}" -gt 0 ] && echo 1 || echo 0)
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
    echo "==> Relay units: ${RELAY_UNITS[*]}"

    echo "==> Stopping relay(s)..."
    for unit in "${RELAY_UNITS[@]}"; do
        sudo systemctl stop "$unit" || true
    done

    echo "==> Building relay..."
    go build -o narrowcast-relay ./cmd/relay

    echo "==> Installing binary..."
    sudo cp narrowcast-relay /usr/local/bin/narrowcast-relay

    # Refresh the template unit in case it changed in this update.
    if [ -f deploy/narrowcast-relay@.service ]; then
        sudo cp deploy/narrowcast-relay@.service /etc/systemd/system/
        sudo systemctl daemon-reload
    fi

    echo "==> Starting relay(s)..."
    for unit in "${RELAY_UNITS[@]}"; do
        sudo systemctl start "$unit"
    done

    echo "==> Done."
    for unit in "${RELAY_UNITS[@]}"; do
        sudo systemctl status "$unit" --no-pager -l || true
    done
fi
