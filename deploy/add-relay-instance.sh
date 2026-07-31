#!/bin/bash
# add-relay-instance.sh — stand up an additional narrowcast relay on its own
# UDP port, alongside any relay(s) already running on this host.
#
# Each instance is one upstream Pi/SDR. They share the cert + domain installed
# by install.sh and differ only by port; clients pick a stream via host:PORT.
# No L4/SNI proxy needed — the kernel demuxes UDP by port.
#
# Usage:  sudo ./add-relay-instance.sh <PORT>
# Example: sudo ./add-relay-instance.sh 8443
set -euo pipefail

PORT="${1:-}"
if [ -z "$PORT" ] || ! [[ "$PORT" =~ ^[0-9]+$ ]] || [ "$PORT" -lt 1 ] || [ "$PORT" -gt 65535 ]; then
    echo "Usage: sudo $0 <PORT>   (1-65535, e.g. 8443)" >&2
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="/etc/narrowcast-relay/relay-${PORT}.env"
UNIT="narrowcast-relay@${PORT}"

if [ ! -f /usr/local/bin/narrowcast-relay ]; then
    echo "narrowcast-relay binary not found. Run install.sh first." >&2
    exit 1
fi
if [ ! -f /etc/narrowcast-relay/certs/server.crt ]; then
    echo "No cert at /etc/narrowcast-relay/certs/. Run install.sh first." >&2
    exit 1
fi

# Install the template unit (idempotent).
sudo cp "${SCRIPT_DIR}/narrowcast-relay@.service" /etc/systemd/system/
sudo systemctl daemon-reload

# Per-instance env: own UPLINK_KEY + CLIENT_PASSWORD (this is a distinct
# upstream from any other relay on the box).
if [ ! -f "$ENV_FILE" ]; then
    read -rp "UPLINK_KEY for the Pi feeding port ${PORT}: " uplink_key
    read -rp "CLIENT_PASSWORD for clients on port ${PORT}: " client_password
    sudo tee "$ENV_FILE" > /dev/null <<ENVEOF
UPLINK_KEY=${uplink_key}
CLIENT_PASSWORD=${client_password}
ENVEOF
    sudo chown narrowcast:narrowcast "$ENV_FILE"
    sudo chmod 600 "$ENV_FILE"
    echo "==> Created ${ENV_FILE}"
else
    echo "==> ${ENV_FILE} already exists — leaving untouched."
fi

# Open the UDP port if ufw is active.
if command -v ufw >/dev/null 2>&1 && sudo ufw status | grep -q "Status: active"; then
    sudo ufw allow "${PORT}/udp" >/dev/null
    echo "==> ufw: allowed ${PORT}/udp"
else
    echo "==> ufw not active — ensure UDP/${PORT} is open in your firewall/security group."
fi

sudo systemctl enable "$UNIT"
sudo systemctl restart "$UNIT"
echo "==> Started ${UNIT}"
echo
echo "Done. Status:  sudo systemctl status ${UNIT}"
echo "Point Pi-B's uplink at  <this-host>:${PORT}  with the matching UPLINK_KEY."
echo "Clients connect to       <domain>:${PORT}  with the matching CLIENT_PASSWORD."
