import Foundation
import Combine
import SwiftUI
import NarrowcastProtocol
import NarrowcastClient

// ConnectionViewModel owns a NarrowcastClient and fans the inbound event
// stream into @Published properties. Audio decode runs off-main on
// AudioPipeline's queue so the SwiftUI runloop isn't pegged at ~50
// packets/sec; only UI-bound state crosses MainActor.
@MainActor
final class ConnectionViewModel: ObservableObject {

    enum State: Equatable {
        case idle
        case connecting(stage: String)
        case connected
        case authFailed
        case error(String)
        case disconnected
    }

    @Published var state: State = .idle
    @Published var serverInfo: NarrowcastClient.ServerInfo?
    @Published var freqHz: UInt64 = 0
    @Published var mode: DemodMode = .nfm
    @Published var sMeterDb: Float = -120
    @Published var squelchDb: Float = -80
    @Published var clientCount: UInt8 = 0
    @Published var streaming: Bool = false  // local audio output gate
    @Published var autoGain: Bool = true
    @Published var manualGainDb: Float = 20

    /// ID of the currently-connected server. Lets the UI no-op when the user
    /// picks the same server they're already on, instead of tearing down +
    /// reconnecting.
    @Published var connectedServerId: UUID?
    @Published var lastLoss: LossTracker.Sample?

    // Stable name for lock-screen Now Playing artist field. Captured at
    // connect() time so we don't need to thread ServerStore in here.
    private var connectedServerName: String = ""
    private let nowPlaying = NowPlayingController()

    // Waterfall data is only published when a consumer asks for it. SwiftUI
    // would otherwise redraw on every 10 Hz FFT regardless of visibility.
    @Published var waterfallFrames: [[UInt8]] = []
    var waterfallEnabled = false
    private let waterfallDepth = 120

    private var client: NarrowcastClient?
    private var pump: Task<Void, Never>?
    // Stable handle the pump captures once. Pipeline mutations (e.g. mode
    // change rebuilding the decoder for a new sample rate) update the
    // contents without invalidating the pump's reference.
    private let pipelineHolder = AudioPipelineHolder()

    func connect(server: Server, password: String?) {
        // Already on this server: re-entry from navigation, no-op.
        if connectedServerId == server.id {
            if state == .connected { return }
            if case .connecting = state { return }
        }
        // Switching servers: tear down the old connection first.
        if connectedServerId != nil, connectedServerId != server.id {
            disconnect()
        }
        if case .connecting = state { return }
        if state == .connected { return }
        connectedServerId = server.id
        connectedServerName = server.name

        // Wire lock-screen play/pause + interruption handling once. Closures
        // capture self weakly via the @MainActor methods.
        nowPlaying.onPlay = { [weak self] in self?.startStreaming() }
        nowPlaying.onPause = { [weak self] in self?.stopStreaming() }
        nowPlaying.activate()

        // Relay always demands auth as the first datagram. Without a password
        // we'd ship Hello, the relay would close us with "unexpected", and
        // the failure surfaces as opaque "Network is down" / EINVAL.
        if server.requiresPassword && (password?.isEmpty ?? true) {
            state = .error("Password required for this server")
            return
        }

        state = .connecting(stage: "Connecting…")

        // Tear down any stale client from a previous failed attempt — Apple
        // QUIC accumulates internal NWConnectionGroup state per attempt and
        // hits POSIX 12 (Cannot allocate memory) after a few stacked
        // failures. Closing the prior client before dialing again releases
        // the buffers.
        if let stale = self.client {
            self.client = nil
            Task { await stale.close() }
        }

        let mode: QUICTransport.Mode = server.allowSelfSigned ? .acceptUnverified : .verifyDefault
        let cfg = NarrowcastClient.Config(
            host: server.host,
            port: server.port,
            password: server.requiresPassword ? password : nil,
            mode: mode
        )
        let client = NarrowcastClient(config: cfg)
        self.client = client

        Task { [weak self] in
            do {
                let info = try await client.connect { stage in
                    Task { @MainActor in
                        if case .connecting = self?.state {
                            self?.state = .connecting(stage: stage)
                        }
                    }
                }
                await MainActor.run {
                    self?.serverInfo = info
                    self?.state = .connected
                    self?.bootAudio(sampleRate: 16000)
                    self?.refreshNowPlaying()
                }
                // Settle: Apple QUIC on the simulator sometimes rejects an
                // immediate post-handshake send with EINVAL. Half a second
                // is enough for the QUIC state machine to commit before we
                // queue CmdStart.
                try? await Task.sleep(nanoseconds: 500_000_000)
                try? await client.send(.start)
                await MainActor.run { self?.streaming = true }
                let holder = await MainActor.run { self?.pipelineHolder }
                guard let holder else { return }
                await self?.runEventPump(client: client, holder: holder)
            } catch NarrowcastClient.ConnectError.authFailed {
                // Close transport on auth fail too — keeps Apple's QUIC
                // group state from leaking into the next attempt.
                await client.close()
                await MainActor.run {
                    if self?.client === client { self?.client = nil }
                    self?.connectedServerId = nil
                    self?.state = .authFailed
                }
            } catch {
                await client.close()
                await MainActor.run {
                    if self?.client === client { self?.client = nil }
                    self?.connectedServerId = nil
                    self?.state = .error(String(describing: error))
                }
            }
        }
    }

    func disconnect() {
        pump?.cancel()
        pump = nil
        pipelineHolder.stop()
        connectedServerId = nil
        nowPlaying.deactivate()
        let c = client
        client = nil
        Task {
            await c?.close()
            await MainActor.run {
                self.state = .disconnected
                self.streaming = false
            }
        }
    }

    /// Resume local audio output. The server pipeline keeps streaming
    /// regardless — this is a client-side mute toggle, not a CmdStop. With
    /// a multi-client relay deployment, sending CmdStop would force the SDR
    /// to stop for everyone.
    func startStreaming() {
        pipelineHolder.resume()
        streaming = true
        nowPlaying.setPlaying(true)
    }

    /// Pause local audio output without telling the server. Other listeners
    /// keep hearing audio; we just stop pulling from the ring buffer.
    func stopStreaming() {
        pipelineHolder.pause()
        streaming = false
        nowPlaying.setPlaying(false)
    }

    func setFrequency(_ hz: UInt64) {
        freqHz = hz // optimistic; may be reconciled by a status frame
        refreshNowPlaying()
        Task { try? await client?.send(.setFrequency(hz: hz)) }
    }

    func setMode(_ m: DemodMode) {
        mode = m
        refreshNowPlaying()
        Task { try? await client?.send(.setMode(m)) }
    }

    func setSquelch(_ db: Float) {
        squelchDb = db
        Task { try? await client?.send(.setSquelch(dBm: db)) }
    }

    func setAutoGain(_ on: Bool) {
        autoGain = on
        Task { try? await client?.send(.setGain(dB: on ? 0 : manualGainDb)) }
    }

    func setManualGain(_ db: Float) {
        manualGainDb = db
        if !autoGain {
            Task { try? await client?.send(.setGain(dB: db)) }
        }
    }

    /// Apply a Favorite by sending the four commands the preset captures.
    /// QUIC datagrams are unordered, and the previous implementation
    /// dispatched four independent Task {}s — frequency commonly arrived
    /// before mode, the mode change rebuilt the DSP chain, and the tune
    /// was lost. Now: update UI optimistically, then send everything in
    /// one sequential Task so each command awaits the previous send.
    func applyFavorite(_ f: Favorite) {
        // Optimistic UI updates. Reload the audio pipeline if mode change
        // implies a new sample rate (e.g. NFM 16 kHz -> WFM 48 kHz),
        // mirroring the handle(.status) flow.
        let prevMode = mode
        mode = f.mode
        freqHz = f.freqHz
        squelchDb = f.squelchDb
        autoGain = f.gainAuto
        manualGainDb = f.gainDb
        refreshNowPlaying()
        if sampleRate(for: f.mode) != sampleRate(for: prevMode) {
            pipelineHolder.set(try? AudioPipeline(sampleRate: sampleRate(for: f.mode)))
        }

        guard let client else { return }
        Task {
            try? await client.send(.setMode(f.mode))
            try? await client.send(.setFrequency(hz: f.freqHz))
            try? await client.send(.setSquelch(dBm: f.squelchDb))
            try? await client.send(.setGain(dB: f.gainAuto ? 0 : f.gainDb))
        }
    }

    /// Build a Favorite snapshot from the current UI/state.
    func currentSnapshotForFavorite(name: String) -> Favorite {
        Favorite(
            name: name,
            freqHz: freqHz,
            mode: mode,
            squelchDb: squelchDb,
            gainAuto: autoGain,
            gainDb: manualGainDb
        )
    }

    private nonisolated func runEventPump(client: NarrowcastClient,
                                          holder: AudioPipelineHolder) async {
        // Loop runs on a background context (not @MainActor). Audio takes
        // the hot path straight to the AudioPipeline queue with no actor
        // hop. FFT is dropped at the case head while waterfall is disabled
        // (no MainActor.run roundtrip just to read a flag); when waterfall
        // is reintroduced behind a perf-tuned renderer this case will
        // route to it directly.
        for await event in await client.events {
            switch event {
            case .audio(let opus):
                holder.feed(opus)

            case .fft:
                continue

            default:
                await MainActor.run { self.handle(event) }
            }
        }
        await MainActor.run {
            if self.state == .connected { self.state = .disconnected }
        }
    }

    private func appendFFT(_ bins: [UInt8]) {
        waterfallFrames.insert(bins, at: 0)
        if waterfallFrames.count > waterfallDepth {
            waterfallFrames.removeLast(waterfallFrames.count - waterfallDepth)
        }
    }

    private func sampleRate(for mode: DemodMode) -> Int {
        // Mirrors pkg/protocol DemodMode.AudioRate() on the Go side.
        switch mode {
        case .wfm: return 48000
        case .nfm, .am: return 16000
        }
    }

    private func bootAudio(sampleRate: Int) {
        pipelineHolder.set(try? AudioPipeline(sampleRate: sampleRate))
    }

    private func refreshNowPlaying() {
        guard state == .connected else { return }
        nowPlaying.updateMetadata(
            stationName: connectedServerName,
            freqHz: freqHz,
            mode: mode,
            playing: streaming
        )
    }

    private func handle(_ event: NarrowcastClient.Event) {
        switch event {
        case .welcome(let info):
            serverInfo = info

        case .status(let s, let q, let m, let f, let cc):
            sMeterDb = s
            squelchDb = q

            if m != mode {
                let prev = mode
                mode = m
                let needRate = sampleRate(for: m)
                let prevRate = sampleRate(for: prev)
                if needRate != prevRate {
                    pipelineHolder.set(try? AudioPipeline(sampleRate: needRate))
                }
            }
            // Older Pi builds emit status without a freq field, decoded as 0.
            // Don't let that overwrite the optimistic local value the user
            // just set via tap-to-tune or the freq sheet.
            if f != 0 {
                freqHz = f
            }
            if let cc { clientCount = cc }
            refreshNowPlaying()

        case .loss(let sample):
            lastLoss = sample

        case .audio, .fft, .unknown:
            // Audio + FFT handled in the pump directly.
            break

        case .disconnected:
            state = .disconnected
            streaming = false
        }
    }
}
