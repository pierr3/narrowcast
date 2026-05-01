# Deployment guide

Narrowcast ships as three independent Go binaries that you can mix and match depending on how you want to listen. None of them are Pi-specific — anything that runs Linux, macOS, or another modern Unix and has a USB RTL-SDR available will work. Raspberry Pi (3B+ / 4 / 5) is the **tested deployment target** and the install scripts are written with it in mind, but the code does not assume Pi hardware anywhere.

## Topologies

You don't have to deploy the whole stack. Pick the shape that matches your use case.

### 1. Standalone (LAN-only)

```
[ SDR host ] -------- QUIC --------> [ client ]
narrowcast                            iOS / CLI / browser
```

A single `narrowcast` process. The client connects directly to it on UDP/4444. Works on any LAN, no public IP, no relay.

When to use:
- You only listen at home / on the same wifi as the SDR host.
- You want the smallest possible setup.
- You don't need multi-client fairness (this topology has no auth and no fan-out — first connect wins).

### 2. Direct over the public Internet

```
[ SDR host with port forward ] -- QUIC --> [ client ]
narrowcast :443
```

Same as standalone, but exposed publicly. Requires a static IP or DDNS, port forwarding for UDP/443 (or whatever port you pick), and a real TLS certificate (otherwise clients hit cert errors). One client at a time, no auth — usable for a private setup but not recommended for shared use.

### 3. Relay deployment (recommended)

```
[ SDR host ] --- narrowcast-uplink ---> [ VPS ] ---> N clients
narrowcast :4444 (loopback)              narrowcast-relay :443
```

The SDR host runs `narrowcast` (loopback only) plus `narrowcast-uplink`. The uplink bridges QUIC datagrams to a `narrowcast-relay` on a VPS with a real DNS name and TLS cert. Clients connect to the relay.

Benefits:
- The SDR host never needs a public IP or port forward — only outbound UDP.
- Multiple clients can listen to one SDR. The relay fans out audio/FFT/status; client tuning commands are forwarded last-writer-wins.
- The relay is pure Go (`CGO_ENABLED=0`) so it runs on any cheap VPS.

This is the default install.sh path and what the README architecture diagram shows.

## Binaries and what they do

| Binary               | Where it runs            | Needs librtlsdr / libopus? |
| -------------------- | ------------------------ | -------------------------- |
| `narrowcast`         | The SDR host             | yes (CGO)                  |
| `narrowcast-uplink`  | The SDR host (relay path)| no                         |
| `narrowcast-relay`   | A public VPS             | no                         |

`narrowcast` is the only one with C dependencies (librtlsdr for the dongle, libopus for the audio encoder). The other two are pure Go and cross-compile trivially.

## Install with `install.sh`

Run from a checkout of the repo on the target host. The script self-detects which set of binaries you want.

```bash
git clone https://github.com/pierr3/narrowcast.git
cd narrowcast
./install.sh
```

It will ask `Choice [1/2]:` —

- **1** — installs `narrowcast-relay` and the systemd unit on this host. Pick this on the VPS.
- **2** — installs `narrowcast` + `narrowcast-uplink` and their systemd units on this host. Pick this on the SDR host.

Pick **2** but then never start the uplink if you want a standalone (LAN-only) deployment. The `narrowcast` service is independent of the uplink.

The script also writes UDP buffer sysctl tuning to `/etc/sysctl.d/99-narrowcast.conf` (`rmem_max=7500000`, `wmem_max=7500000`) and applies it. quic-go warns loudly without these.

## Configuration files

The install script puts each binary's runtime configuration in a per-role directory under `/etc/`. The systemd units load these via `EnvironmentFile=`.

### SDR host — `/etc/narrowcast/`

```
/etc/narrowcast/
├── certs/
│   ├── server.crt        # self-signed, generated on first install
│   └── server.key
└── uplink.env            # only present if you chose to run the uplink
```

`/etc/narrowcast/uplink.env`:

```
RELAY_ADDR=relay.example.com:443
UPLINK_KEY=some-shared-secret
```

- `RELAY_ADDR` — `host:port` of the relay. Hostname is resolved once at uplink startup; see [DNS caching](#dns-caching) below.
- `UPLINK_KEY` — must match the `UPLINK_KEY` set on the relay. SHA-256-hashed before transmission.

To change either value: `sudo nano /etc/narrowcast/uplink.env`, then `sudo systemctl restart narrowcast-uplink`.

### Relay host — `/etc/narrowcast-relay/`

```
/etc/narrowcast-relay/
├── certs/
│   ├── server.crt        # Let's Encrypt fullchain (or your own CA)
│   └── server.key
└── relay.env
```

`/etc/narrowcast-relay/relay.env`:

```
UPLINK_KEY=some-shared-secret
CLIENT_PASSWORD=listener-password
```

- `UPLINK_KEY` — must match the value on the SDR host's uplink.env. SHA-256-hashed before comparison; choose a random value, anything ≥ 24 chars.
- `CLIENT_PASSWORD` — what listening clients enter when they add the server. Same hashing.

To rotate either value: edit the file, restart the service. **You will need to update both ends together.** The uplink will reconnect-loop with `auth failed` until the keys match again.

```bash
sudo nano /etc/narrowcast-relay/relay.env
sudo systemctl restart narrowcast-relay
```

The relay also needs a real TLS cert at `/etc/narrowcast-relay/certs/server.{crt,key}`. The installer will offer to wire up a Let's Encrypt cert + auto-renewal hook; if you skip that, you must place the cert yourself. The renewal hook copies the renewed cert into the certs directory and restarts the relay (the unprivileged `narrowcast` user can't traverse `/etc/letsencrypt/live/`).

## Updating

```bash
ssh user@host
cd ~/narrowcast
./update.sh
```

`update.sh` auto-detects the role from installed systemd units, pulls the latest source, rebuilds the relevant binaries, restarts services. Downtime is ~30 seconds for the SDR side, ~5 seconds for the relay.

You only need to redeploy the relay when a commit touches `cmd/relay`, `pkg/protocol`, or shared types. Pure DSP / SDR-side commits are SDR-host-only.

## DNS caching

The uplink is the only piece that needs to resolve a hostname (the relay's). To survive flaky upstream DNS — `systemd-resolved` is known to wedge briefly during network blips, especially on wifi — the uplink resolves the relay hostname **once at startup** and caches the resulting IP for the lifetime of the process.

The resolution flow:

1. Try the system resolver. On most installs that's `systemd-resolved` listening on `127.0.0.53`.
2. If the system resolver returns an error or times out, try **Cloudflare's public resolver** (`1.1.1.1:53`) directly via Go's pluggable resolver. This bypasses any local resolver issue.
3. Retry with exponential backoff (1 s → 30 s) until one of them succeeds. The systemd unit will not give up — a long network outage at boot is recoverable without manual intervention.

Once the relay's IP is in hand, all subsequent reconnects (whether triggered by network drops, NAT churn, or QUIC idle) **dial the cached IP directly**. They no longer touch DNS at all. TLS certificate validation still works correctly because `tls.Config.ServerName` is set to the original hostname; the certificate's CN/SAN is checked against the hostname even though the connection went to an IP.

What this means in practice:

- A wedged `systemd-resolved` no longer prevents reconnection. You used to see `lookup ...: server misbehaving` errors stalling each reconnect attempt for the full DNS timeout. Those are gone.
- If your relay's IP changes (rare for a VPS), `systemctl restart narrowcast-uplink` re-resolves on next start. There is intentionally no automatic re-resolve while running.
- If your relay's hostname is wrong at install time, the uplink will retry resolving every 1–30 seconds and log each failure. Fix the hostname (`/etc/narrowcast/uplink.env`), restart, and you're back.

A defense-in-depth measure on the SDR host: pin a fallback DNS in `/etc/systemd/resolved.conf` so resolved itself recovers faster from upstream blips:

```
[Resolve]
FallbackDNS=1.1.1.1 9.9.9.9
```

`sudo systemctl restart systemd-resolved`. The uplink doesn't need this — it has its own fallback path — but other things on the host (apt, package mirrors, etc.) will benefit.

## systemd unit files

Stored in `/etc/systemd/system/`:

- `narrowcast.service` — runs `narrowcast` as user `narrowcast`, with `SupplementaryGroups=plugdev` so it can talk to the USB SDR.
- `narrowcast-uplink.service` — runs `narrowcast-uplink` as user `narrowcast`, depends on `narrowcast.service` (`Requires=`/`After=`).
- `narrowcast-relay.service` — runs `narrowcast-relay` as user `narrowcast`, with `AmbientCapabilities=CAP_NET_BIND_SERVICE` so it can bind UDP/443.

To customize the SDR-side flags (e.g. select a specific RTL-SDR by serial when you have multiple dongles):

```bash
sudo systemctl edit narrowcast
```

Drop in an override:

```ini
[Service]
ExecStart=
ExecStart=/usr/local/bin/narrowcast \
    -cert /etc/narrowcast/certs/server.crt \
    -key /etc/narrowcast/certs/server.key \
    -serial 00000002 \
    -host 127.0.0.1
```

`sudo systemctl daemon-reload && sudo systemctl restart narrowcast`.

## Logs

```bash
sudo journalctl -u narrowcast -f             # SDR-side
sudo journalctl -u narrowcast-uplink -f      # bridge
sudo journalctl -u narrowcast-relay -f       # VPS
```

The uplink prints `[uplink] relay <name> resolved to <ip>` at startup once DNS resolution succeeds, then `connected to relay` / `connected to local narrowcast` / `bridging datagrams` once both sides are up. If a side drops, the diagnostic line names which one (`local narrowcast dropped` vs `relay dropped`) so you don't have to guess.

## Reset / clean reinstall

If something is wrong with the installed services (mismatched binary paths, stale configuration), the cleanest path is:

```bash
sudo systemctl stop narrowcast narrowcast-uplink || true
sudo systemctl disable narrowcast narrowcast-uplink || true
sudo rm -f /etc/systemd/system/narrowcast.service
sudo rm -f /etc/systemd/system/narrowcast-uplink.service
sudo rm -rf /etc/systemd/system/narrowcast.service.d
sudo rm -rf /etc/systemd/system/narrowcast-uplink.service.d
sudo rm -f /usr/local/bin/narrowcast /usr/local/bin/narrowcast-uplink
sudo rm -rf /etc/narrowcast              # wipes uplink.env + certs
sudo systemctl daemon-reload
cd ~/narrowcast && git pull && ./install.sh
```

You'll be re-prompted for `RELAY_ADDR` and `UPLINK_KEY`. Cert is regenerated. Same procedure with `narrowcast-relay` (just adjust the paths to the relay equivalents) for a clean VPS reinstall.
