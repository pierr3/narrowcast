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
        echo "==> Created /etc/narrowcast-relay/relay.env"
    else
        echo "==> /etc/narrowcast-relay/relay.env already exists — leaving untouched."
        echo "    To regenerate keys, delete it and rerun this script."
    fi

    echo ""
    echo "TLS cert setup."
    echo "If you have a Let's Encrypt cert for the relay's domain, this script"
    echo "can copy it into /etc/narrowcast-relay/certs/ and install a renewal"
    echo "hook so cert rotation happens automatically. Leave blank to skip and"
    echo "manage certs yourself."
    read -rp "Let's Encrypt domain (blank to skip): " le_domain

    if [ -n "$le_domain" ]; then
        if [ ! -d "/etc/letsencrypt/live/$le_domain" ]; then
            echo "==> WARNING: /etc/letsencrypt/live/$le_domain does not exist."
            echo "    Skipping cert copy. Run certbot first, then rerun this script."
        else
            echo "==> Copying cert + key into /etc/narrowcast-relay/certs/..."
            sudo cp "/etc/letsencrypt/live/$le_domain/fullchain.pem" /etc/narrowcast-relay/certs/server.crt
            sudo cp "/etc/letsencrypt/live/$le_domain/privkey.pem"  /etc/narrowcast-relay/certs/server.key
            sudo chown narrowcast:narrowcast /etc/narrowcast-relay/certs/server.*
            sudo chmod 644 /etc/narrowcast-relay/certs/server.crt
            sudo chmod 600 /etc/narrowcast-relay/certs/server.key

            echo "==> Installing /etc/letsencrypt/renewal-hooks/post/narrowcast-relay.sh"
            sudo mkdir -p /etc/letsencrypt/renewal-hooks/post
            sudo tee /etc/letsencrypt/renewal-hooks/post/narrowcast-relay.sh > /dev/null <<HOOKEOF
#!/bin/bash
# Auto-installed by narrowcast install.sh.
# Copies the renewed Let's Encrypt cert into the location the relay reads
# (the narrowcast user can't traverse /etc/letsencrypt/live), then bounces
# the relay so it picks up the new cert.
set -e
DOMAIN="$le_domain"
cp "/etc/letsencrypt/live/\$DOMAIN/fullchain.pem" /etc/narrowcast-relay/certs/server.crt
cp "/etc/letsencrypt/live/\$DOMAIN/privkey.pem"  /etc/narrowcast-relay/certs/server.key
chown narrowcast:narrowcast /etc/narrowcast-relay/certs/server.*
chmod 644 /etc/narrowcast-relay/certs/server.crt
chmod 600 /etc/narrowcast-relay/certs/server.key
systemctl restart narrowcast-relay
HOOKEOF
            sudo chmod +x /etc/letsencrypt/renewal-hooks/post/narrowcast-relay.sh
            echo "==> Cert in place + renewal hook installed."
        fi
    else
        echo ""
        echo "Skipped LE setup. Place certs manually at:"
        echo "  /etc/narrowcast-relay/certs/server.crt"
        echo "  /etc/narrowcast-relay/certs/server.key"
    fi

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
        echo "==> Self-signed cert created (uplink connects via InsecureSkipVerify)."
    else
        echo "==> /etc/narrowcast/certs/server.crt already exists — leaving untouched."
    fi

    if [ ! -f /etc/narrowcast/uplink.env ]; then
        echo ""
        echo "Uplink connects to the relay over the public network."
        echo "Relay address is host:port — port should match the relay's listen port"
        echo "(443 by default since the recent change)."
        read -rp "Relay address (e.g. relay.example.com:443): " relay_addr
        read -rp "Uplink key (must match the relay's UPLINK_KEY): " uplink_key
        sudo tee /etc/narrowcast/uplink.env > /dev/null <<ENVEOF
RELAY_ADDR=${relay_addr}
UPLINK_KEY=${uplink_key}
ENVEOF
        sudo chown narrowcast:narrowcast /etc/narrowcast/uplink.env
        sudo chmod 600 /etc/narrowcast/uplink.env
        echo "==> Created /etc/narrowcast/uplink.env"
    else
        echo "==> /etc/narrowcast/uplink.env already exists — leaving untouched."
        echo "    To change relay address or key, edit it directly or delete and rerun."
    fi

    echo "==> Installing systemd services..."
    sudo cp deploy/narrowcast.service /etc/systemd/system/
    sudo cp deploy/narrowcast-uplink.service /etc/systemd/system/
    sudo systemctl daemon-reload
    sudo systemctl enable narrowcast narrowcast-uplink

    echo "==> (Re)starting services..."
    # Restart, not just start — covers the rerun case where binaries changed
    # but services were already up. Plain start is a no-op when already active.
    sudo systemctl restart narrowcast
    sudo systemctl restart narrowcast-uplink

    echo ""
    echo "Done. Check status with:"
    echo "  sudo systemctl status narrowcast"
    echo "  sudo systemctl status narrowcast-uplink"
    echo ""
    echo "Tail logs with:"
    echo "  sudo journalctl -u narrowcast -u narrowcast-uplink -f"
    ;;
*)
    echo "Invalid choice." >&2
    exit 1
    ;;
esac
