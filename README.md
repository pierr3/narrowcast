# Narrowcast — Lightweight RTL-SDR Streaming Server

A Go server that runs on a Raspberry Pi, demodulates RTL-SDR signals, and streams compressed audio + waterfall data to clients over QUIC.

## Why

SpyServer is closed-source. SDR++ server streams raw IQ (high bandwidth). Narrowcast does server-side demodulation and sends only:

- **Opus audio** (~4 KB/s) — NFM, WFM, AM
- **FFT waterfall** (~3-5 KB/s) — 1024 u8 bins at 10 fps
- **Total: ~10 KB/s** — works on bad WiFi, cellular, anything

Everything is encrypted via QUIC (TLS 1.3). No VPN needed.

## Architecture

```txt
RTL-SDR → librtlsdr → [FIR xlating filter] → [FM/AM demod] → [Opus encode] → QUIC datagrams
                     → [FFT 1024pt]         → [u8 magnitude bins]            → QUIC datagrams
                                              [commands]                      ← QUIC stream 0
```

## Requirements

- Go 1.23+
- librtlsdr (`apt install librtlsdr-dev` on Pi, `brew install librtlsdr` on macOS)
- libopus (`apt install libopus-dev` on Pi, `brew install opus` on macOS)
- RTL-SDR USB dongle

## Quick Start

```bash
# Generate TLS certs
make certs

# Build and run
make run
```

## Cross-compile for Raspberry Pi

```bash
# Requires cross-compiler toolchain
make build-pi

# Copy to Pi
scp bin/narrowcast-arm64 pi@<pi-ip>:~/narrowcast
scp -r certs/ pi@<pi-ip>:~/certs/
```

## Protocol

QUIC connection on UDP port 4444 (default).

| Channel  | Transport                  | Content                           |
| -------- | -------------------------- | --------------------------------- |
| Commands | QUIC stream (reliable)     | Length-prefixed binary messages   |
| Audio    | QUIC datagram (unreliable) | `0x01` + Opus packet              |
| FFT      | QUIC datagram (unreliable) | `0x02` + u16 bin count + u8 bins  |
| Status   | QUIC datagram (unreliable) | `0x03` + s-meter + squelch + mode |

## Demodulation Modes

| Mode | Bandwidth | Audio Rate | Use Case             |
| ---- | --------- | ---------- | -------------------- |
| NFM  | 12.5 kHz  | 16 kHz     | Amateur, PMR, marine |
| WFM  | 200 kHz   | 48 kHz     | Broadcast FM         |
| AM   | 10 kHz    | 16 kHz     | Aviation band        |

## Configuration

```txt
Usage: narrowcast [flags]
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

## Pi Setup

```bash
# Install deps
sudo apt install librtlsdr-dev libopus-dev

# Increase UDP buffer (add to /etc/sysctl.conf)
sudo sysctl -w net.core.rmem_max=7500000

# Run
./narrowcast --host 0.0.0.0 --port 4444
```
