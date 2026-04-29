# Narrowcast — Lightweight RTL-SDR Streaming Server

A Go server that runs on a Raspberry Pi, demodulates RTL-SDR signals server-side, and streams compressed audio + waterfall data over QUIC to clients on the worst networks they can find.

## Goal

Build the smallest, most resilient SDR server for **remote stations on bad networks**: a Pi at a relative's house, a beach cabin on LTE, a mountain repeater on 4G, a boat on satellite. The constraint is the uplink, not the radio.

Design priorities, in order:

1. **Bandwidth-cheap**: total stream ~14 KB/s (~110 kbps). Works on 3G, EDGE-with-headroom, congested wifi, satellite.
2. **Loss-tolerant**: graceful behavior under packet loss and jitter — single dropped audio packets are reconstructed, not silenced.
3. **Modern transport**: QUIC + TLS 1.3 + datagrams. Single UDP port. Connection migration for handoffs between cell towers / wifi.
4. **Low footprint**: ~2.5K LOC, ~1 CPU core on a Pi 5, ~10 MB RAM. No heavy dependencies.
5. **Basic features**: NFM, WFM, AM, S-meter, waterfall, live tuning. The 80% of what a listener actually does.

## Why not the existing tools

- **SpyServer**: closed-source, runs over TCP, no datagram support, no FEC, head-of-line blocking when a packet is late.
- **SDR++ server**: streams raw IQ — multi-megabit per client, dead on a mobile uplink.
- **OpenWebRX**: web-only, heavy server, no encrypted-by-default transport.

Narrowcast does server-side demodulation and sends only the finished audio + a downsampled waterfall, all encrypted, all over QUIC datagrams.

## Bandwidth budget

| Stream    | Size      | Notes                                          |
| --------- | --------- | ---------------------------------------------- |
| Opus audio| ~4 KB/s   | 32 kbps mono + in-band FEC for ~10% loss       |
| FFT       | ~10 KB/s  | 1024 u8 bins × 10 fps                          |
| Status    | ~70 B/s   | S-meter, mode, freq, squelch, client count     |
| **Total** | **~14 KB/s** | **~110 kbps — fits 3G / weak LTE / sat**       |

Reference: SpyServer ~200 KB/s. SDR++ raw IQ: 4–10 Mbps. Narrowcast is 15–500× lighter.

## Architecture

```txt
                    ┌── Pi (cellular / wifi) ─────────────────────┐
                    │  RTL-SDR ──► librtlsdr                      │
                    │       ──► xlating FIR + decimate            │
                    │       ──► FM/AM demod                       │
                    │       ──► AGC (hang-time on AM)             │
                    │       ──► Opus + FEC ──► QUIC datagrams ────┼──┐
                    │       ──► FFT 1024pt  ──► QUIC datagrams ───┼──┤
                    │           commands     ◄── QUIC datagrams ──┼──┤
                    └─────────────────────────────────────────────┘  │
                                                                     │
                              QUIC over UDP, TLS 1.3, single port    │
                                                                     │
                                                                     ▼
                                              ┌──── relay (VPS) ─────────┐
                                              │ auth, fan-out, multiplex │
                                              └──────────────────────────┘
                                                          │
                                                          ▼
                                                ┌──── clients ─────┐
                                                │  any platform    │
                                                └──────────────────┘
```

A typical deployment: `narrowcast` (server-side demod) on the Pi at the antenna, `narrowcast-uplink` bridging to a `narrowcast-relay` on a VPS with a real DNS name, clients connecting to the relay over QUIC. The Pi only needs a single outbound UDP connection — no port forwarding, no public IP.

## Resilience

The design assumes the network is bad and tries to fail gracefully:

- **QUIC datagrams** for audio/FFT/status — a dropped packet does not head-of-line-block the next one (the dominant SpyServer-over-TCP failure mode).
- **Opus in-band FEC**: each frame carries a low-bitrate copy of the previous frame. A single dropped audio packet is reconstructed, not silenced. Costs ~25% bitrate, recovers transparent under loss up to ~10%.
- **Flush-on-retune**: changing frequency drains stale IQ and resets every DSP stage so the listener hears a clean cut, not a transient sweep.
- **Reset-on-drop**: when the SDR pipeline can't keep up (slow client, congested network), DSP state is cleared at the boundary instead of filtering across a discontinuity. Brief silence beats sustained warbling artifacts.
- **Hang-time AGC for AM**: gain freezes during dead air, so the first syllable of an aviation transmission isn't clipped while attack catches up.
- **Adaptive bitrate / FFT throttle**: the server emits a `SeqMark` datagram once per second carrying its monotonic send-counts. Clients diff against their own receive counts and report measured loss back via `CmdQualityReport`. The server then steps Opus from 32 → 24 → 16 kbps and FFT from 10 → 5 → 2 → 1 fps based on loss. **16 kbps is a hard floor** — below that, voice quality drops below acceptable and we'd rather rely on QUIC's natural cutout than ship muddy audio. Old clients that don't send reports stay at full quality; the system is purely advisory and never blocks the audio path.
- **Reconnect-with-backoff** on the uplink and clients.

## Demodulation modes

| Mode | Channel BW | Audio Rate | Use case                  |
| ---- | ---------- | ---------- | ------------------------- |
| NFM  | 16 kHz     | 16 kHz     | Amateur, PMR, marine VHF  |
| WFM  | 200 kHz    | 48 kHz     | Broadcast FM              |
| AM   | 25 kHz     | 16 kHz     | Aviation band (full ICAO) |

## Quick start

```bash
# macOS deps
brew install librtlsdr opus opusfile pkg-config

# Pi deps
sudo apt install librtlsdr-dev libopus-dev libopusfile-dev

# Generate self-signed TLS certs
make certs

# Build + run
make run
```

## Cross-compile for Pi from macOS

```bash
make build-pi           # produces bin/narrowcast-arm64
scp bin/narrowcast-arm64 pi@<pi-ip>:~/narrowcast
scp -r certs/ pi@<pi-ip>:~/certs/
```

## Update a running deployment

```bash
ssh pi
cd ~/narrowcast
./update.sh             # auto-detects SDR vs relay role
```

The script stops the relevant systemd unit, rebuilds, installs, restarts. ~30 sec downtime for the SDR side, ~5 sec for the relay.

Rule of thumb: **only redeploy the relay when a commit touches `cmd/relay`, `pkg/protocol`, or shared types**. Pure DSP / SDR-side commits are Pi-only.

## Protocol

QUIC over UDP, TLS 1.3, single port (default 4444). Datagrams for everything — commands, audio, telemetry. No reliable streams.

| Datagram type | Direction      | Payload                                           |
| ------------- | -------------- | ------------------------------------------------- |
| `0x01` audio  | server→client  | Opus packet                                       |
| `0x02` FFT    | server→client  | `[u16 numBins][u8 bins...]`                       |
| `0x03` status | server→client  | `[f32 smeter][f32 squelch][u8 mode][u64 freq]…`   |
| `0x04` seqmark| server→client  | `[u32 audioSent][u32 fftSent][u32 statusSent]` (1/s)|
| `0x10` setfreq| client→server  | `[u64 freqHz]`                                    |
| `0x11` setmode| client→server  | `[u8 mode]`                                       |
| `0x12` squelch| client→server  | `[f32 dBm]`                                       |
| `0x13` setgain| client→server  | `[f32 dB]` (0 = auto)                             |
| `0x14` quality| client→server  | `[u8 audioLossPct][u8 fftLossPct][u16 windowMs]`  |
| `0x20` start  | client→server  | (none)                                            |
| `0x21` stop   | client→server  | (none)                                            |
| `0x30` hello  | client→server  | `[u8 protoVer]`                                   |
| `0x31` welcome| server→client  | `[u8 ver][u64 minHz][u64 maxHz][f32 sampleRate]`  |
| `0x32`/`0x33`/`0x34` | client/relay | password / uplink-key auth (SHA-256)         |

## Configuration

```txt
narrowcast [flags]
  --host          Listen address (default: 0.0.0.0)
  --port          Listen port (default: 4444)
  --cert          TLS cert file (default: certs/server.crt)
  --key           TLS key file (default: certs/server.key)
  --samplerate    RTL-SDR sample rate (default: 2400000)
  --device        RTL-SDR device index (default: 0)
  --fftsize       FFT bin count (default: 1024)
  --fftrate       FFT frames/sec (default: 10)
  --opus-bitrate  Opus bitrate in bps (default: 32000)
```

## Pi setup notes

```bash
# Install C deps
sudo apt install librtlsdr-dev libopus-dev libopusfile-dev

# Increase UDP buffer (add to /etc/sysctl.conf for persistence)
sudo sysctl -w net.core.rmem_max=7500000

# Run
./narrowcast --host 0.0.0.0 --port 4444
```

## Status

Working: NFM/WFM/AM, S-meter, waterfall, live tuning, multi-client via relay (last-writer-wins on tuning), Opus FEC, hang-time AM AGC, flush + reset on glitches.

On the roadmap, in priority order:

1. **Per-client virtual VFO** — each client tunes independently within the hardware's capture window via per-client NCO. Multiple listeners on the same Pi without contention.
2. **Connection-quality status field** — surface measured loss / current bitrate to the client UI so users see "weak signal" instead of silently degraded audio.
3. **SSB (USB/LSB)** — for HF amateur with an upconverter.
