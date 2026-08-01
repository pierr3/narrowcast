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

Tests cover the pure logic where a silent regression would be expensive: `pkg/dsp` (filters checked against direct convolution and for block-size invariance, FFT/bin pooling), `pkg/protocol` (the non-blocking datagram writer), `pkg/config` (sample-rate validation). Run with `go test ./pkg/...`; the writer tests are worth running under `-race`. There is no coverage of the QUIC plumbing or the DSP chain end to end — verify those with `--simulate` (synthetic IQ source in `pkg/sdr/simulate.go`) plus `cmd/testclient`, which needs no radio.

`--pprof localhost:6060` exposes `net/http/pprof`. Use it before optimizing anything on the Pi; the thermal budget is the real constraint and guesses about where the cycles go have been wrong before.

**The pipeline reports its own load once a minute** (`[pipeline] health: … % of one core, mean/worst ms against the 20 ms block budget, iq queue, drops`), and that line is the first thing to read when someone says the audio is laggy. It exists because the two obvious measurements both mislead: total CPU on a four-core Pi reads ~25 % while the single pipeline goroutine is saturated, and SoC temperature says nothing about whether that goroutine made its deadline. The load is also bimodal — the Opus encoder only runs while the squelch is open — so a channel that idles comfortably can be over budget for the whole of every transmission. `HEALTH WARNING` in place of `health` means past 70 % of a core or a single block over budget. If the line reads a few percent, the lag is not on the Pi, and the next place to look is the client's own latency readout.

**Opus encode is the most expensive stage in the pipeline, not the DSP.** At complexity 9 (libopus's own choice for this configuration) one 20 ms frame of 16 kHz mono measured 106 µs against 71 µs for the entire wideband channel filter; complexity 5 measured 51 µs for output that is indistinguishable at voice bitrates, because the extra search depth pays off on wideband music at high bitrates and finds almost nothing when the bitrate is the binding constraint. Hence `--opus-complexity` defaulting to 5. Anyone hunting Pi cycles should look here before micro-optimising a filter.

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

- `0x01–0x06` server→client data (`Audio`, `FFT`, `Status`, `SeqMark`, `AudioSeq`, `Pong`)
- `0x10–0x35` commands (`SetFrequency`, `SetMode`, `SetSquelch`, `SetGain`, `QualityReport`, `Ping`, `Start`, `Stop`, `Hello`, `Welcome`, auth)

Relay fan-out sends every server→client datagram to **every** client, so any
per-client reply needs a token the requester can recognise — that's why `Ping`
carries one and clients ignore pongs they didn't ask for.

When extending the protocol: assign a new type byte, document the payload layout in a comment above the constant, ensure unknown types are silently ignored on both sides (older clients/relays must keep working). The relay forwards datagrams it doesn't recognize unchanged.

**Never call `quic.Connection.SendDatagram` directly from a data path.** It *blocks* once 32 datagram frames are queued (quic-go `datagram_queue.go`), and every producer here is realtime, so blocking propagates backwards into the SDR: pipeline blocks → stops draining `iqChan` → SDR callback drops buffers → DSP resets → seconds of broken audio from one brief hiccup. In the relay it was worse, since one slow client stalled the fan-out loop and therefore every listener plus the radio itself. Use `protocol.Writer`, which owns the blocking send in its own goroutine and sheds the oldest queued datagram instead of waiting (FFT frames first, audio last). Direct sends are fine only for handshake replies on a fresh connection.

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

Three modes' worth of chain, but **one pipeline for the whole device**, not one per client. `serverState` reference-counts listeners (connections that sent `CmdStart`) and every output datagram is broadcast to all subscribers. It used to be per-client, with all of them reading the same `iqChan` — two connections then stole alternating IQ blocks from each other, so both got half the sample stream and both produced broken audio, at double the CPU. The relay path hides this (one upstream connection) except during an uplink reconnect overlap. Keep the pipeline shared.

A few non-obvious invariants:

- **Squelch gates on channel power, never on audio level.** `dsp.Squelch` reads the filtered RF channel before demodulation, because an AM carrier and an FM envelope are both steady for a whole transmission while *audio* level dips between syllables. Gating on audio (as this did originally) makes the threshold impossible to set: it chatters mid-sentence. The S-meter reports the same quantity, so the number on the meter is the number to aim the slider at — don't change one without the other. Hysteresis absorbs noise at the set point; hang time bridges speech gaps; a *threshold change* deliberately bypasses hysteresis so dragging the slider above a signal mutes it immediately.
- **AM carrier tracking is what allows a narrow filter.** The wide channel filter stays wide so offset-carrier ground stations (several kHz off the nominal channel) are captured at all; `dsp.FineTuner` then shifts whichever carrier is transmitting to DC and filters ±3.5 kHz around it. That buys **46 dB** of adjacent-channel rejection, measured — in Europe the neighbouring 8.33 kHz channel is only 8.33 kHz away, and the wide filter rejected it by just 8 dB, so neighbours bled in as apparent hiss. The search window comes from the channel plan (`airbandSearchHz`) and must stay narrower than half the channel spacing, or an 8.33 channel locks onto its neighbour. Note the fine tuner runs *after* decimation, at 48 kHz: centring at the SDR rate would force complex taps onto the most expensive loop in the program.
- **AM uses `AudioAGC` (hang-time), not `AGC`** — standard AGC ramps gain into the noise floor between pushes-to-talk and clips the start of the next transmission. Hang-time freezes gain during dead air. Don't change AM to regular AGC.
- **The tuner's DC offset is removed before anything reads the IQ** (`dsp.IQDCBlocker`). Zero-IF LO leakage puts a constant offset in the stream, i.e. a permanent spike at exactly the tuned frequency, and it misleads four stages at once: it is the tallest bar in the spectrum, `FindCarrierOffset` locks onto it instead of an offset-carrier ground station, `ChannelPowerDb` reads high so the S-meter and squelch are biased, and it adds a fake steady carrier to the AM envelope that the AGC then divides by. Two rules keep it correct. The estimate is **only refined while the squelch is shut** — a carrier tuned dead on frequency is also at DC, so a continuously-adapting blocker eats the wanted signal. And it is refined **unconditionally for the first 100 blocks**, because leakage is what inflates channel power: without priming, an uncorrected receiver reads a dead channel as busy, holds the squelch open, and never gets a quiet block to learn from. The simulator models the leakage deliberately (`dcLeakageI/Q`); its absence is why carrier tracking looked perfect in simulation while failing on every real dongle.
- **Signal presence is decided once, by the squelch, on channel power.** `AudioAGC` used to make its own call via an absolute envelope floor, and that was a bug: every level in the chain scales with the RF gain setting, so turning the tuner down from maximum put the envelope permanently under the floor and froze the AGC at unity gain forever. Don't reintroduce an absolute level threshold anywhere downstream of the tuner gain. `AudioAGC.Restart()` is separate from `Reset()` — the pipeline calls it on each squelch *opening*, because consecutive transmissions are different stations and an aircraft overhead can be 20 dB above a distant controller.
- **AM skips the soft limiter** — amplitude IS the audio in AM, so tanh compression distorts the voice.
- **`dspChain.Reset()` is called on two events**: hardware retune (drains stale IQ from `iqChan` + zeros every filter history), and SDR drop (the callback dropped buffers because the pipeline couldn't keep up). Continuing to filter across a discontinuity produces audible warbling. Any new stateful DSP stage must implement `Reset()`.
- **The SDR drop counter** (`state.dropCount`, an `atomic.Uint64`) is the only signal that the IQ stream had a gap. If you add a new DSP entry point, sync the `lastDrops` baseline before processing.
- **DSP stages own their output buffers.** Anything returning a slice (`XlatingFilter`, `RealFIRFilter`, the demodulators, `CU8ToComplexInto`, `MagnitudeToBins`) hands back storage it reuses on the next call; callers must not retain it. This is what makes a steady-state block allocation-free, which matters because the GC cost of the old ~40 MB/s of churn was a real slice of the Pi's thermal budget. A new stage that allocates per block breaks that property silently.
- **The sample rate must be an exact multiple of every mode's audio rate** (i.e. of 48000), enforced by `config.Validate`. Decimation is integer division, so 1.024 MS/s silently yields WFM audio clocked 1.6 % wrong. Default is 960 kS/s; raising it scales the channel filter's cost roughly linearly.
- **`iqWanted` gates the SDR callback.** With no listeners nothing copies IQ out of the USB ring — idle heat for nobody.

## Bandwidth-adaptive feedback loop

The server emits `DatagramSeqMark` (type `0x04`) once per second carrying `[u32 audioSent][u32 fftSent][u32 statusSent]`. Clients diff against their own receive counts and report measured loss back via `CmdQualityReport` (`0x14`). The pipeline reacts in `runPipeline`'s `qualityChan` case:

- `adaptFFTInterval(base, lossPct)` — steps from configured rate down to 1/10 (e.g. 10 → 1 fps)
- `adaptOpusBitrate(base, lossPct)` — steps 32 → 24 → 20 → **16 kbps floor**. Below 16 kbps, voice quality drops below acceptable for the monitoring use case; below the floor we'd rather rely on QUIC's natural cutout than ship muddy audio.
- `adaptOpusLossPerc(lossPct)` — `lossPct + 5`, clamped 5–50, fed to `opus.SetPacketLossPerc` so FEC redundancy tracks observed loss.

Old clients that don't report stay at full quality; the loop is purely advisory and never blocks the audio path.

The FEC bits this pays for are only redeemable if the client can tell a lost packet from a closed squelch, which is what `DatagramAudioSeq` (`0x05`) is for — see `docs/PROTOCOL.md`. The seq counters in `SeqMark` deliberately count datagrams *queued*, including ones `protocol.Writer` sheds locally, so server-side shedding also shows up as measured loss.

## Operational gotchas

- **UDP buffers**: `quic-go` wants 7.5 MB receive buffers. Linux defaults to ~208 KiB → noisy "failed to sufficiently increase receive buffer size" warning and bursts get dropped. `install.sh` writes `/etc/sysctl.d/99-narrowcast.conf` with `net.core.rmem_max=7500000` / `net.core.wmem_max=7500000` before installing anything. If shipping a code change that affects buffering on the Pi side, verify the sysctl is still applied.
- **Relay redeploy is rare**: only redeploy the relay when a commit touches `cmd/relay`, `pkg/protocol`, or shared types. Pure DSP / SDR-side commits are Pi-only. `update.sh` auto-detects role from installed systemd units.
- **TLS cert paths**: relay reads from `/etc/narrowcast-relay/certs/server.{crt,key}`. The installer writes a Let's Encrypt renewal hook at `/etc/letsencrypt/renewal-hooks/post/narrowcast-relay.sh` that copies the renewed cert into that directory and bounces the service — needed because the unprivileged `narrowcast` user can't traverse `/etc/letsencrypt/live/`.
- **Last-writer-wins on tuning**: any connected client can `SetFrequency` / `SetMode`. There is no per-client virtual VFO yet (it's the top item on the README roadmap); a frequency change affects everyone.
- **IQ block size must be a multiple of 512.** librtlsdr silently replaces any other `buf_len` with its own 256 KiB default — the old `sampleRate/10*2` request was in fact getting 137 ms blocks. `iqBufBytes` targets 20 ms rounded down to 512 B.

## Editing rules of thumb

- Prefer editing `pkg/dsp` over adding new packages. The DSP code is the load-bearing surface; keep it small and explicit.
- New protocol fields: add the constant + payload doc in `pkg/protocol/protocol.go`, update the README protocol table and `docs/PROTOCOL.md`, ensure server and client tolerate the absence of the field on the other side.
- On the Pi side, hot-path work is measured in per-block microseconds against a 20 ms budget, and allocation counts as work. `go test -bench . ./pkg/dsp` reports both; keep `allocs/op` at zero for the steady state.
- Don't introduce reliable streams for runtime data. Datagrams are the design — head-of-line blocking on a slow stream is the failure mode being avoided.
- The relay must remain pure Go (`CGO_ENABLED=0`) so it builds for any VPS without C deps.
