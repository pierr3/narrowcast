# Narrowcast — Lightweight RTL-SDR Streaming Server

A Go server that demodulates RTL-SDR signals server-side and streams compressed audio + waterfall data over QUIC to clients on the worst networks they can find.

## Goal

Build the smallest, most resilient SDR server for **remote stations on bad networks**: an SDR host at a relative's house, a beach cabin on LTE, a mountain repeater on 4G, a boat on satellite. The constraint is the uplink, not the radio.

Design priorities, in order:

1. **Bandwidth-cheap**: total stream ~14 KB/s (~110 kbps). Works on 3G, EDGE-with-headroom, congested wifi, satellite.
2. **Loss-tolerant**: graceful behaviour under packet loss and jitter — single dropped audio packets are reconstructed, not silenced.
3. **Modern transport**: QUIC + TLS 1.3 + datagrams. Single UDP port. Connection migration for handoffs between cell towers / wifi.
4. **Low footprint**: ~2.5K LOC, ~1 CPU core on a Pi 5, ~10 MB RAM. No heavy dependencies.
5. **Basic features**: NFM, WFM, AM, S-meter, waterfall, live tuning. The 80% of what a listener actually does.

> Narrowcast is built for and tested on Raspberry Pi (3B+ / 4 / 5) with an RTL-SDR. The code is plain Go and runs on any modern Linux/macOS host with a USB SDR — the install scripts and the rest of this README mention Pi as the canonical deployment target, but nothing in the code is Pi-specific.

## Why not the existing tools

- **SpyServer**: closed-source, runs over TCP, no datagram support, no FEC, head-of-line blocking when a packet is late.
- **SDR++ server**: streams raw IQ — multi-megabit per client, dead on a mobile uplink.
- **OpenWebRX**: web-only, heavy server, no encrypted-by-default transport.

Narrowcast does server-side demodulation and sends only the finished audio + a downsampled waterfall, all encrypted, all over QUIC datagrams.

## Bandwidth budget

| Stream    | Size      | Notes                                          |
| --------- | --------- | ---------------------------------------------- |
| Opus audio| ~4 KB/s   | 32 kbps mono + in-band FEC for ~10% loss       |
| FFT       | ~2.6 KB/s | 256 u8 bins × 10 fps, max-pooled from a 1024-point FFT |
| Status    | ~200 B/s  | S-meter, mode, freq, squelch, client count @ 10 Hz |
| **Total** | **~7 KB/s** | **~56 kbps — fits 3G / weak LTE / sat**        |

Reference: SpyServer ~200 KB/s. SDR++ raw IQ: 4–10 Mbps. Narrowcast is 15–500× lighter.

## Deployment topologies

You don't need the whole stack. Pick the shape that matches your use case.

### Standalone (LAN-only)

```
[ SDR host ] -------- QUIC --------> [ client ]
narrowcast :4444
```

Just `narrowcast` on the SDR host, client connects directly. No relay, no uplink, no auth. Good for listening at home over your own wifi.

### Direct over the public internet

Same as standalone but the SDR host has a public IP / DDNS + port forward. Single client, no auth — workable for a private setup, not for shared use.

### Relay (recommended for remote use)

```
                          QUIC over UDP/443, TLS 1.3
[ SDR host ] -- narrowcast-uplink -----------------> [ VPS ] --> N clients
narrowcast :4444 (loopback)                          narrowcast-relay
```

The SDR host runs `narrowcast` (loopback) plus `narrowcast-uplink`. The uplink bridges QUIC datagrams to a `narrowcast-relay` on a VPS with a real DNS name and TLS cert. Clients connect to the relay.

Why this topology:
- The SDR host needs only outbound UDP — no public IP, no port forward.
- Multiple clients can listen to one SDR. The relay fans out audio/FFT/status; client tuning commands are forwarded last-writer-wins.
- The relay listens on **UDP/443** by default. QUIC's standard port (HTTP/3) gets through ~85% of restrictive networks because they whitelist QUIC; random high UDP ports get blocked closer to half the time.

For configuration files, paths, DNS caching behaviour, and clean-reinstall steps, see [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md).

## Resilience

The design assumes the network is bad and tries to fail gracefully:

- **QUIC datagrams** for audio/FFT/status — a dropped packet does not head-of-line-block the next one (the dominant SpyServer-over-TCP failure mode).
- **Opus in-band FEC**: each frame carries a low-bitrate copy of the previous frame. A single dropped audio packet is reconstructed, not silenced. Costs ~25% bitrate, recovers transparent under loss up to ~10%.
- **Flush-on-retune**: changing frequency drains stale IQ and resets every DSP stage so the listener hears a clean cut, not a transient sweep.
- **Reset-on-drop**: when the SDR pipeline can't keep up (slow client, congested network), DSP state is cleared at the boundary instead of filtering across a discontinuity. Brief silence beats sustained warbling artifacts.
- **Hang-time AGC for AM**: gain freezes during dead air, so the first syllable of an aviation transmission isn't clipped while attack catches up.
- **Adaptive bitrate / FFT throttle**: the server emits a `SeqMark` datagram once per second; clients diff their own counts to compute loss and report back via `CmdQualityReport`. The server then steps Opus 32 → 24 → 16 kbps and FFT 20 → 1 fps based on loss. **16 kbps is a hard floor** — below that, voice quality drops below acceptable and we'd rather rely on QUIC's natural cutout than ship muddy audio. Old clients that don't send reports stay at full quality.
- **Reconnect-with-backoff** on the uplink and clients.
- **Cached DNS on the uplink**: relay hostname is resolved once at startup, with Cloudflare as a fallback if the system resolver is misbehaving. All subsequent reconnects dial the cached IP — a wedged `systemd-resolved` no longer stalls recovery. TLS still validates against the original hostname via SNI. Details in [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md#dns-caching).

## Demodulation modes

| Mode | Channel BW | Audio Rate | Use case                  |
| ---- | ---------- | ---------- | ------------------------- |
| NFM  | 16 kHz     | 16 kHz     | Amateur, PMR, marine VHF  |
| WFM  | 200 kHz    | 48 kHz     | Broadcast FM              |
| AM   | 25 kHz     | 16 kHz     | Aviation band (full ICAO) |

## Quick start (development)

```bash
# macOS deps
brew install librtlsdr opus opusfile pkg-config

# Linux / Pi deps
sudo apt install librtlsdr-dev libopus-dev libopusfile-dev

# Generate self-signed TLS certs
make certs

# Build + run
make run
```

Connect a client to `localhost:4444`. The repo includes `cmd/testclient` for CLI debugging and a SwiftUI iOS / macOS client under `clients/apple/`.

## Production install

For a real deployment (relay or SDR host), use the install script — it handles users, sysctl tuning, certs, env files, and systemd:

```bash
git clone https://github.com/pierr3/narrowcast.git
cd narrowcast
./install.sh
```

It will ask whether this host is the relay (VPS) or the SDR host; pick accordingly. See [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) for everything else: env file paths, key rotation, custom flags, DNS, troubleshooting.

## Cross-compile for Pi from macOS

```bash
make build-pi           # produces bin/narrowcast-arm64
scp bin/narrowcast-arm64 pi@<pi-ip>:~/narrowcast
```

## Update a running deployment

```bash
ssh user@host
cd ~/narrowcast
./update.sh             # auto-detects relay vs SDR-host role
```

The script stops the relevant systemd unit, rebuilds, installs, restarts. ~30 sec downtime for the SDR side, ~5 sec for the relay.

## Protocol summary

QUIC over UDP, TLS 1.3, single port. Datagrams for everything — commands, audio, telemetry. No reliable streams.

| Datagram type | Direction      | Payload                                           |
| ------------- | -------------- | ------------------------------------------------- |
| `0x01` audio  | server→client  | Opus packet (legacy form, no sequence number)     |
| `0x05` audio  | server→client  | `[u16 seq][Opus packet]` — the default; seq is what lets the client redeem the encoder's in-band FEC |
| `0x02` FFT    | server→client  | `[u16 numBins][u8 bins...]`                       |
| `0x03` status | server→client  | `[f32 smeter][f32 squelch][u8 mode][u64 freq][u8 cc?]` |
| `0x04` seqmark| server→client  | `[u32 audioSent][u32 fftSent][u32 statusSent]` (1/s)|
| `0x10` setfreq| client→server  | `[u64 freqHz]`                                    |
| `0x11` setmode| client→server  | `[u8 mode]`                                       |
| `0x12` squelch| client→server  | `[f32 dBm]`                                       |
| `0x13` setgain| client→server  | `[f32 dB]` (0 = auto)                             |
| `0x06` pong   | server→client  | `[u32 token]` — echo of a ping, for RTT timing     |
| `0x14` quality| client→server  | `[u8 audioLossPct][u8 fftLossPct][u16 windowMs]`  |
| `0x15` ping   | client→server  | `[u32 token]`                                     |
| `0x20` start  | client→server  | (none)                                            |
| `0x21` stop   | client→server  | (none)                                            |
| `0x30` hello  | client→server  | `[u8 protoVer]`                                   |
| `0x31` welcome| server→client  | `[u8 ver][u64 minHz][u64 maxHz][f32 sampleRate]`  |
| `0x32`/`0x33`/`0x34`/`0x35` | various | password / uplink-key auth (SHA-256)        |

For full payload semantics, byte-level layouts, and protocol-versioning rules, see [`docs/PROTOCOL.md`](docs/PROTOCOL.md).

## Configuration

```txt
narrowcast [flags]
  --host          Listen address (default: 0.0.0.0)
  --port          Listen port (default: 4444)
  --cert          TLS cert file (default: certs/server.crt)
  --key           TLS key file (default: certs/server.key)
  --serial        RTL-SDR device serial (overrides --device)
  --device        RTL-SDR device index (default: 0)
  --samplerate    RTL-SDR sample rate, must be a multiple of 48000 (default: 960000)
  --fftsize       FFT length, power of two (default: 1024)
  --fftbins       Bins transmitted per frame, max-pooled from fftsize (default: 256)
  --fftrate       FFT frames/sec (default: 10)
  --squelch-hysteresis  dB below the threshold before the gate closes (default: 3)
  --squelch-hang        ms to hold the gate open after signal drops (default: 500)
  --am-carrier-track    Follow the carrier within an AM channel (default: true)
  --am-bandwidth        Narrow AM filter half-bandwidth in Hz (default: 3500)
  --am-presence         dB of presence lift on the AM consonant band (default: 5, 0 disables)
  --am-nr               AM noise reduction: 0 off, 1 light, 2 medium, 3 strong (default: 2)
  --opus-bitrate  Opus bitrate in bps (default: 32000)
  --opus-complexity     Opus encoder complexity 0-10 (default: 5)
  --audio-seq     Sequence-numbered audio datagrams (default: true)
  --pprof         Serve net/http/pprof on this address, e.g. localhost:6060
  --simulate      Use simulated SDR (no hardware needed)
```

## Status

Working: NFM/WFM/AM, S-meter (with peak-hold), spectrum graph, live tuning, multi-client via relay (last-writer-wins), Opus FEC, hang-time AM AGC, flush + reset on glitches, lock-screen / Now Playing controls on the iOS client, iCloud-synced server list and favourites, background audio.

On the roadmap, in priority order:

1. **Per-client virtual VFO** — each client tunes independently within the hardware's capture window via per-client NCO. Multiple listeners on the same SDR without contention.
2. **SSB (USB/LSB)** — for HF amateur with an upconverter.
3. **Mac client** — same Swift code base as iOS, separate target.

## Documentation

- [`docs/PROTOCOL.md`](docs/PROTOCOL.md) — full wire protocol reference (datagram + command formats, byte-level layout, versioning rules).
- [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) — install paths, env files, key rotation, DNS caching, systemd overrides, clean-reinstall steps.
- [`CLAUDE.md`](CLAUDE.md) — project conventions for code contributions.
