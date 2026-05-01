# Narrowcast Wire Protocol

This document specifies the wire format for the QUIC link between a `narrowcast` server, a `narrowcast-relay`, an uplink, and clients. All multi-byte integers are **little-endian** unless noted otherwise.

## Transport

- **QUIC** (RFC 9000) over UDP, single port.
- **TLS 1.3**. The relay uses a real CA-issued certificate (typically Let's Encrypt). A standalone `narrowcast` deployment uses a self-signed cert (the uplink connects with `InsecureSkipVerify`).
- ALPN: `narrowcast-v1`.
- **Datagrams enabled** (RFC 9221). All runtime traffic — audio, FFT, status, telemetry, commands — is carried as QUIC datagrams. There are no reliable streams for runtime data; head-of-line blocking is the failure mode the design avoids.
- Default port: **UDP/443** for the relay (looks like HTTP/3 to firewalls — survives most restrictive networks), **UDP/4444** for the LAN-only `narrowcast` server.
- Datagram MTU: clients must tolerate any payload up to the negotiated `max_datagram_frame_size` (typically ≥ 1200 B). Unknown datagram type bytes must be silently ignored on both sides — this is how the protocol gains new fields without breaking older peers.

## Datagram framing

Every datagram begins with a single **type byte** identifying the message. The remainder is the payload, format determined by type. There is no length prefix because the QUIC datagram already carries its own length.

```
+--------+---------------------+
| type B | payload (0..N B)    |
+--------+---------------------+
```

## Type byte ranges

| Range       | Direction        | Meaning                    |
| ----------- | ---------------- | -------------------------- |
| `0x01–0x04` | server → client  | Streaming data + telemetry |
| `0x10–0x14` | client → server  | Tuning + adaptation control|
| `0x20–0x21` | client → server  | Streaming gate (start/stop)|
| `0x30–0x35` | mixed            | Handshake + auth           |

## Server → client datagrams

### `0x01` Audio

Encoded Opus packet (raw — no length prefix). One packet per encoded audio frame; frame durations are 20 ms by default.

```
+------+-------------------+
| 0x01 | opus packet bytes |
+------+-------------------+
```

The server adapts Opus bitrate (32 → 24 → 16 kbps) based on `CmdQualityReport` from the client. **16 kbps is a hard floor** — below that, voice quality drops below acceptable for the monitoring use case.

### `0x02` FFT

Down-sampled magnitude spectrum of the wideband IQ stream. The server emits at `--fftrate` fps (default 20), throttled down to 1 fps under heavy loss.

```
+------+-----------------+----------------+
| 0x02 | u16 numBins LE  | u8 bins[N]     |
+------+-----------------+----------------+
```

- `numBins` is the count that follows. With `--fftsize 1024`, this is 1024 bins.
- Each bin is a dBFS magnitude mapped to `0..255` by `MagnitudeToU8` in `pkg/dsp/fft.go`. The mapping is `byte = clamp((dBFS + 120) * 255/120, 0, 255)`, so:
  - `0` ≡ -120 dBFS or below
  - `255` ≡ 0 dBFS
- The bin order is **DC-centered** (FFT-shifted server-side). Bin 0 is the lowest frequency in the captured passband, bin `numBins-1` is the highest, bin `numBins/2` is at the tuned center frequency.
- The captured passband width equals the SDR sample rate (default 2.4 MHz).

### `0x03` Status

Periodic telemetry frame. Default rate 20 Hz.

```
+------+----------------+----------------+----------+----------------+----------+
| 0x03 | f32 smeter dBm | f32 squelch dBm| u8 mode  | u64 freq Hz LE | u8 cc?   |
+------+----------------+----------------+----------+----------------+----------+
```

- `smeter` is the post-channel-filter signal level in dBm. Range typically `-120..0`. Used for the S-meter UI.
- `squelch` is the current threshold in dBm.
- `mode` is the active `DemodMode` enum (`0=NFM`, `1=WFM`, `2=AM`).
- `freq` is the current center frequency in Hz.
- `cc` is **optional** — appended by the relay before fan-out as the connected-client count, clamped to 255. Standalone (no-relay) deployments emit this field as zero-length, i.e. the datagram is 21 bytes total instead of 22. Clients must tolerate either length.

### `0x04` SeqMark

Quality-feedback anchor. Sent **once per second** by the server. Carries monotonic per-stream send counters since the pipeline started. Clients diff against their own receive counts to compute loss percentages.

```
+------+----------------+----------------+----------------+
| 0x04 | u32 audioSent  | u32 fftSent    | u32 statusSent |
+------+----------------+----------------+----------------+
```

All three counters are little-endian and reset to zero when the pipeline starts (typically on first `CmdStart`).

The client should respond with a `CmdQualityReport` once it has measured a window worth of data (e.g. 2–5 seconds).

## Client → server datagrams

### `0x10` SetFrequency

```
+------+----------------+
| 0x10 | u64 freq Hz LE |
+------+----------------+
```

Server retunes the SDR. The DSP pipeline drains stale IQ and resets every stateful filter so the listener hears a clean cut, not a transient sweep.

### `0x11` SetMode

```
+------+---------+
| 0x11 | u8 mode |
+------+---------+
```

Mode values: `0=NFM`, `1=WFM`, `2=AM`. Other values are silently ignored. Mode change rebuilds the DSP chain and switches the audio sample rate (NFM/AM 16 kHz, WFM 48 kHz). Clients must rebuild their audio decoder accordingly — the next Opus frames will be at the new rate.

### `0x12` SetSquelch

```
+------+--------------+
| 0x12 | f32 dBm      |
+------+--------------+
```

Sets the squelch threshold. Below this, audio is muted at the server (no datagrams emitted). Range `-120..0`.

### `0x13` SetGain

```
+------+--------------+
| 0x13 | f32 dB       |
+------+--------------+
```

Sets the RTL-SDR tuner gain. `0` enables auto. RTL-SDR exposes a discrete table of supported gains (0 / 0.9 / 1.4 / … / 49.6 dB); the driver picks the closest.

### `0x14` QualityReport

Loss measurement reported by the client.

```
+------+-------------------+----------------+----------------+
| 0x14 | u8 audioLossPct   | u8 fftLossPct  | u16 windowMs   |
+------+-------------------+----------------+----------------+
```

- Loss percentages are 0–100, computed as `100 * (sent - received) / sent` over the most recent window.
- `windowMs` is the duration of the window in milliseconds (typically 1000–5000).
- Reports are advisory. The server uses them to step Opus bitrate (32 → 24 → 16 kbps), FFT rate (20 → 1 fps), and Opus FEC redundancy. A client that never reports stays at full quality.

### `0x20` Start / `0x21` Stop

Empty payloads. Gates whether the server actively pumps audio + FFT + status frames.

```
+------+
| 0x20 |
+------+
```

`Start` causes the server to begin streaming on the current frequency/mode. `Stop` halts it. Both are idempotent.

In a relay deployment the relay sends `Stop` upstream automatically when the last client disconnects, so the SDR stops processing samples while no one is listening.

### `0x30` Hello

Client identification + handshake.

```
+------+-----------+
| 0x30 | u8 protoV |
+------+-----------+
```

- `protoVersion` is currently `1`. The server replies with a `Welcome` carrying its supported version; clients with a different version may still negotiate or fail gracefully.
- The server emits a `Welcome` on **every** received Hello (clients may resend Hello on welcome timeout — common after a relay reconnects with stale upstream state).

## Server → client handshake

### `0x31` Welcome

```
+------+-----------+----------------+----------------+----------------+
| 0x31 | u8 protoV | u64 minHz LE   | u64 maxHz LE   | f32 sampleRate |
+------+-----------+----------------+----------------+----------------+
```

- `minHz` / `maxHz`: tuner range. The reference implementation reports `24_000_000 .. 1_766_000_000` (RTL-SDR R820T2 typical).
- `sampleRate`: the SDR's IQ sample rate in samples/sec. Determines FFT span and bin width.

## Relay-only datagrams

These messages are exchanged only when a `narrowcast-relay` sits between the client and the SDR.

### `0x32` Auth (client → relay)

Sent **before** anything else when connecting via relay. The relay closes the connection if the first datagram is not `0x32` (client) or `0x35` (uplink).

```
+------+-----------------------------+
| 0x32 | 32 B SHA-256(password)      |
+------+-----------------------------+
```

The relay compares against a SHA-256 of `CLIENT_PASSWORD` from its environment and replies with `AuthOK` (`0x33`) or `AuthFail` (`0x34`).

### `0x33` AuthOK / `0x34` AuthFail (relay → client/uplink)

Empty payloads. After `AuthOK`, normal datagrams flow.

### `0x35` Uplink (uplink → relay)

```
+------+-----------------------------+
| 0x35 | 32 B SHA-256(uplinkKey)     |
+------+-----------------------------+
```

Used by `narrowcast-uplink` to register itself as the upstream source for fan-out. The relay holds at most one upstream at a time; a new authenticated uplink replaces any prior one.

## Fan-out (relay)

Once an uplink has authenticated and one or more clients have authenticated, the relay forwards datagrams in both directions:

- **upstream → all clients**: every datagram from the uplink is broadcast to every authenticated client. The relay rewrites Status frames to append the connected-client count byte.
- **client → upstream**: every datagram from any client is forwarded to the upstream. The relay does not arbitrate — last-writer-wins on tuning. Per-client virtual VFOs are on the roadmap.

When the last client disconnects, the relay sends `0x21 Stop` to the upstream so the SDR stops sampling.

## Adaptation flow (server-side)

```
client receives audio + fft + status
  → counts datagrams locally
  → server emits SeqMark every 1s
  → client diffs counts → loss %
  → client sends CmdQualityReport every 2s
  → server adapts:
        Opus bitrate     32 → 24 → 16 kbps
        FFT frame rate   base → base/2 → base/5 → base/10
        Opus FEC perc    audioLossPct + 5, clamped 5..50
```

The adaptation is purely advisory; old clients that never report stay at full quality. Audio is never blocked on the loop.

## Protocol versioning

`ProtoVersion` is currently `1`. New datagram types or fields are added by:

1. Allocating a new type byte in the appropriate range (`0x05–0x0F` for server-side, `0x15–0x1F` for client-side, `0x36+` for handshake).
2. Documenting the payload here and in `pkg/protocol/protocol.go`.
3. Ensuring **both server and client tolerate the absence** of the new field on the other side. Relays forward datagrams they don't recognize unchanged, so they need no update for new payload-only changes.

The hard rule: never break older peers. Length-extending an existing payload (as Status did with the optional client-count byte) is the preferred way to add fields, since old parsers stop at the byte they expected and ignore the extra bytes.
