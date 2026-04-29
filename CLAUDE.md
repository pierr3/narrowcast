# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`narrowcast` is a Go server that runs on a Raspberry Pi, demodulates RTL-SDR signals server-side, and streams Opus audio + downsampled FFT over QUIC datagrams to clients on bad networks (mobile, weak wifi, satellite). The hard constraint is the **uplink budget (~14 KB/s total)**, not the radio. Every design choice falls out of that.

When evaluating changes, ask: *does this preserve the bandwidth budget, the loss-tolerance, and the small footprint?* Heavy abstractions, extra dependencies, or perf optimizations that don't reduce bandwidth or improve resilience usually don't fit.

## Build / run

Three Go binaries, two of which are pure Go (no CGO). `narrowcast` (the SDR server) needs librtlsdr + libopus — CGO required.

```bash
make build           # narrowcast (current host; needs librtlsdr + libopus)
make build-relay     # cmd/relay   — pure Go, runs on VPS
make build-uplink    # cmd/uplink  — pure Go, runs on Pi alongside narrowcast
make build-pi        # cross-compile narrowcast for arm64 Pi (needs aarch64-linux-gnu-gcc)
make certs           # self-signed TLS for local dev
make run             # build + run on :4444 with self-signed certs
```

macOS deps: `brew install librtlsdr opus opusfile pkg-config`. Pi/Linux: `sudo apt install librtlsdr-dev libopus-dev libopusfile-dev`. The Makefile injects `CGO_CFLAGS`/`CGO_LDFLAGS` for Homebrew paths on macOS.

There is no test suite. Manual verification with `--simulate` (synthetic IQ source in `pkg/sdr/simulate.go`) avoids needing real hardware.

## Three-process topology

```
   Pi: [narrowcast] <─QUIC localhost─ [uplink] ──QUIC over public net──> [relay] (VPS) ──> clients
```

- **`cmd/narrowcast`** — the SDR demod server. Owns the RTL-SDR, runs the DSP pipeline, listens on QUIC. Self-signed cert; the uplink connects with `InsecureSkipVerify`.
- **`cmd/uplink`** — pure datagram bridge: connects to local narrowcast and to the remote relay, forwards datagrams in both directions. Authenticates to the relay with a SHA-256 hash of `UPLINK_KEY`. Reconnect-with-backoff loop.
- **`cmd/relay`** — VPS-side authenticator and fan-out. Real Let's Encrypt cert. First datagram on a connection is either `CmdUplink` (Pi auth) or `CmdAuth` (client auth, hash of `CLIENT_PASSWORD`). Holds one upstream + N clients; multiplexes server→client datagrams, forwards client→server commands (last-writer-wins on tuning). When the last client disconnects, sends `CmdStop` upstream so the SDR stops.
- **`cmd/testclient`** — local debug client.

The relay listens on **UDP/443** (QUIC standard port; survives more hostile firewalls than random high UDP). Binding <1024 from the unprivileged `narrowcast` user requires `AmbientCapabilities=CAP_NET_BIND_SERVICE` in the systemd unit — already set in `deploy/narrowcast-relay.service`.

## Wire protocol

Single QUIC connection per peer; **datagrams for everything** — commands, audio, FFT, telemetry. There are no reliable streams used for runtime data. See `pkg/protocol/protocol.go` for the canonical list. Two type-byte ranges:

- `0x01–0x04` server→client data (`Audio`, `FFT`, `Status`, `SeqMark`)
- `0x10–0x35` commands (`SetFrequency`, `SetMode`, `SetSquelch`, `SetGain`, `QualityReport`, `Start`, `Stop`, `Hello`, `Welcome`, auth)

When extending the protocol: assign a new type byte, document the payload layout in a comment above the constant, ensure unknown types are silently ignored on both sides (older clients/relays must keep working). The relay forwards datagrams it doesn't recognize unchanged.

## DSP pipeline

`cmd/narrowcast/main.go` is the orchestrator. The DSP chain is built per-mode in `buildDSPChain()`:

```
CU8 IQ → xlating FIR (mix + decimate) → demod (FM/AM)
       → soft limiter (FM only)
       → audio decimation FIR (anti-aliased)
       → de-emphasis (FM)
       → voice bandpass 400-3000 Hz (AM only)
       → AGC (FM regular / AM hang-time AudioAGC)
       → Opus encode → QUIC datagrams
```

Three modes (`pkg/protocol/protocol.go`): `NFM` (16 kHz / 16 kHz audio), `WFM` (200 / 48), `AM` (25 / 16, full ICAO aviation channel).

A few non-obvious invariants:

- **AM uses `AudioAGC` (hang-time), not `AGC`** — standard AGC ramps gain into the noise floor between pushes-to-talk and clips the start of the next transmission. Hang-time freezes gain during dead air. Don't change AM to regular AGC.
- **AM skips the soft limiter** — amplitude IS the audio in AM, so tanh compression distorts the voice.
- **`dspChain.Reset()` is called on two events**: hardware retune (drains stale IQ from `iqChan` + zeros every filter history), and SDR drop (the callback dropped buffers because the pipeline couldn't keep up). Continuing to filter across a discontinuity produces audible warbling. Any new stateful DSP stage must implement `Reset()`.
- **The SDR drop counter** (`state.dropCount`, an `atomic.Uint64`) is the only signal that the IQ stream had a gap. If you add a new DSP entry point, sync the `lastDrops` baseline before processing.

## Bandwidth-adaptive feedback loop

The server emits `DatagramSeqMark` (type `0x04`) once per second carrying `[u32 audioSent][u32 fftSent][u32 statusSent]`. Clients diff against their own receive counts and report measured loss back via `CmdQualityReport` (`0x14`). The pipeline reacts in `runPipeline`'s `qualityChan` case:

- `adaptFFTInterval(base, lossPct)` — steps from configured rate down to 1/10 (e.g. 10 → 1 fps)
- `adaptOpusBitrate(base, lossPct)` — steps 32 → 24 → **16 kbps floor**. Below 16 kbps, voice quality drops below acceptable for the monitoring use case; below the floor we'd rather rely on QUIC's natural cutout than ship muddy audio.
- `adaptOpusLossPerc(lossPct)` — `lossPct + 5`, clamped 5–50, fed to `opus.SetPacketLossPerc` so FEC redundancy tracks observed loss.

Old clients that don't report stay at full quality; the loop is purely advisory and never blocks the audio path.

## Operational gotchas

- **UDP buffers**: `quic-go` wants 7.5 MB receive buffers. Linux defaults to ~208 KiB → noisy "failed to sufficiently increase receive buffer size" warning and bursts get dropped. `install.sh` writes `/etc/sysctl.d/99-narrowcast.conf` with `net.core.rmem_max=7500000` / `net.core.wmem_max=7500000` before installing anything. If shipping a code change that affects buffering on the Pi side, verify the sysctl is still applied.
- **Relay redeploy is rare**: only redeploy the relay when a commit touches `cmd/relay`, `pkg/protocol`, or shared types. Pure DSP / SDR-side commits are Pi-only. `update.sh` auto-detects role from installed systemd units.
- **TLS cert paths**: relay reads from `/etc/narrowcast-relay/certs/server.{crt,key}`. The installer writes a Let's Encrypt renewal hook at `/etc/letsencrypt/renewal-hooks/post/narrowcast-relay.sh` that copies the renewed cert into that directory and bounces the service — needed because the unprivileged `narrowcast` user can't traverse `/etc/letsencrypt/live/`.
- **Last-writer-wins on tuning**: any connected client can `SetFrequency` / `SetMode`. There is no per-client virtual VFO yet (it's the top item on the README roadmap); a frequency change affects everyone.

## Editing rules of thumb

- Prefer editing `pkg/dsp` over adding new packages. The DSP code is the load-bearing surface; keep it small and explicit.
- New protocol fields: add the constant + payload doc in `pkg/protocol/protocol.go`, update the README protocol table, ensure server and client tolerate the absence of the field on the other side.
- Don't introduce reliable streams for runtime data. Datagrams are the design — head-of-line blocking on a slow stream is the failure mode being avoided.
- The relay must remain pure Go (`CGO_ENABLED=0`) so it builds for any VPS without C deps.
