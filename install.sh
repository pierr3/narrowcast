#!/usr/bin/env bash
set -euo pipefail

echo "Narrowcast installer"
echo "===================="
echo ""
echo "What would you like to install?"
echo "  1) Relay only (no hardware deps, runs on VPS)"
echo "  2) SDR server + uplink (runs on Pi with RTL-SDR)"
echo ""
read -rp "Choice [1/2]: " choice

case "$choice" in
1)
    echo ""
    echo "==> Creating narrowcast user..."
    if ! id narrowcast &>/dev/null; then
        sudo useradd -r -s /usr/sbin/nologin narrowcast
    fi

    echo "==> Building relay..."
    go build -o narrowcast-relay ./cmd/relay
    sudo cp narrowcast-relay /usr/local/bin/narrowcast-relay
    echo "==> Installed /usr/local/bin/narrowcast-relay"

    echo "==> Setting up config directory..."
    sudo mkdir -p /etc/narrowcast-relay/certs

    if [ ! -f /etc/narrowcast-relay/relay.env ]; then
        echo ""
        read -rp "Uplink key (Pi authenticates with this): " uplink_key
        read -rp "Client password (listeners authenticate with this): " client_pw
        sudo tee /etc/narrowcast-relay/relay.env > /dev/null <<ENVEOF
UPLINK_KEY=${uplink_key}
CLIENT_PASSWORD=${client_pw}
ENVEOF
        sudo chown narrowcast:narrowcast /etc/narrowcast-relay/relay.env
        sudo chmod 600 /etc/narrowcast-relay/relay.env
    fi

    echo ""
    echo "TLS certs: place your cert and key at:"
    echo "  /etc/narrowcast-relay/certs/server.crt"
    echo "  /etc/narrowcast-relay/certs/server.key"
    echo "(e.g. from Let's Encrypt)"

    echo "==> Installing systemd service..."
    sudo cp deploy/narrowcast-relay.service /etc/systemd/system/
    sudo systemctl daemon-reload
    sudo systemctl enable narrowcast-relay

    echo ""
    echo "Done. Start with: sudo systemctl start narrowcast-relay"
    ;;
2)
    echo ""
    echo "==> Creating narrowcast user..."
    if ! id narrowcast &>/dev/null; then
        sudo useradd -r -s /usr/sbin/nologin -G plugdev narrowcast
    fi

    echo "==> Building narrowcast + uplink..."
    go build -o narrowcast ./cmd/narrowcast
    go build -o narrowcast-uplink ./cmd/uplink

    echo "==> Installing binaries..."
    sudo cp narrowcast /usr/local/bin/narrowcast
    sudo cp narrowcast-uplink /usr/local/bin/narrowcast-uplink

    echo "==> Setting up config directory..."
    sudo mkdir -p /etc/narrowcast/certs

    if [ ! -f /etc/narrowcast/certs/server.crt ]; then
        echo "==> Generating self-signed TLS cert for local SDR server..."
        sudo openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
            -keyout /etc/narrowcast/certs/server.key \
            -out /etc/narrowcast/certs/server.crt \
            -days 3650 -nodes -subj "/CN=narrowcast"
        sudo chown narrowcast:narrowcast /etc/narrowcast/certs/*
        sudo chmod 600 /etc/narrowcast/certs/server.key
    fi

    if [ ! -f /etc/narrowcast/uplink.env ]; then
        echo ""
        read -rp "Relay address (host:port): " relay_addr
        read -rp "Uplink key: " uplink_key
        sudo tee /etc/narrowcast/uplink.env > /dev/null <<ENVEOF
RELAY_ADDR=${relay_addr}
UPLINK_KEY=${uplink_key}
ENVEOF
        sudo chown narrowcast:narrowcast /etc/narrowcast/uplink.env
        sudo chmod 600 /etc/narrowcast/uplink.env
    fi

    echo "==> Installing systemd services..."
    sudo cp deploy/narrowcast.service /etc/systemd/system/
    sudo cp deploy/narrowcast-uplink.service /etc/systemd/system/
    sudo systemctl daemon-reload
    sudo systemctl enable narrowcast narrowcast-uplink

    echo "==> Starting services..."
    sudo systemctl start narrowcast
    sudo systemctl start narrowcast-uplink

    echo ""
    echo "Done. Check status with:"
    echo "  sudo systemctl status narrowcast"
    echo "  sudo systemctl status narrowcast-uplink"
    ;;
*)
    echo "Invalid choice." >&2
    exit 1
    ;;
esac
