import Foundation
import Combine
import QuartzCore
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

    /// S-meter level lives on its own observable object, deliberately.
    ///
    /// Status frames arrive ~10×/s, and while these were @Published on the view
    /// model every frame invalidated the entire ListenView body: the favourites
    /// bar, the segmented picker, three String(format:) calls and an
    /// updateUIView on the Metal view, all rebuilt to move one meter. Scoping
    /// the fast-changing values to a leaf object confines that redraw to the
    /// meter itself.
    let meter = MeterState()

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

    // Shared with SpectrumView. The pump feeds bins on the background context;
    // the Metal renderer aggregates them into bars on demand.
    let spectrumStore = SpectrumStore()

    private var client: NarrowcastClient?
    /// The connect + event-pump task. Held so disconnect can actually cancel it
    /// — this used to be a fire-and-forget `Task {}` whose handle was dropped,
    /// which made disconnect()'s cancel a no-op and let a stale pump keep
    /// feeding audio into a pipeline that had already been replaced.
    private var sessionTask: Task<Void, Never>?
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

        sessionTask = Task { [weak self] in
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
        sessionTask?.cancel()
        sessionTask = nil
        // Pending slider sends would otherwise land on the next connection.
        squelchSendTask?.cancel()
        squelchSendTask = nil
        gainSendTask?.cancel()
        gainSendTask = nil
        pipelineHolder.stop()
        connectedServerId = nil
        // Don't leave a stale needle and spectrum behind on the next connect.
        meter.reset()
        spectrumStore.reset()
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

    /// Trailing-edge debouncers for slider-driven commands. SwiftUI fires
    /// the binding setter on every drag delta — without this the server
    /// log is a wall of identical-modulo-noise updates, the QUIC datagram
    /// queue fills with stale tunings, and the encoder churns settings
    /// it'll immediately overwrite. Cancelling the prior pending task
    /// each call means only the final value (or one every 150 ms while
    /// the user pauses mid-drag) reaches the wire.
    private var squelchSendTask: Task<Void, Never>?
    private var gainSendTask: Task<Void, Never>?
    private static let sliderDebounceNs: UInt64 = 150_000_000

    func setSquelch(_ db: Float) {
        squelchDb = db
        squelchSendTask?.cancel()
        squelchSendTask = Task { [weak self] in
            try? await Task.sleep(nanoseconds: Self.sliderDebounceNs)
            if Task.isCancelled { return }
            try? await self?.client?.send(.setSquelch(dBm: db))
        }
    }

    func setAutoGain(_ on: Bool) {
        autoGain = on
        // Toggle, not a slider — fire immediately. Cancel any pending
        // manual-gain send so an in-flight slider value doesn't land
        // after the toggle and re-enable manual gain on the server.
        gainSendTask?.cancel()
        gainSendTask = nil
        Task { try? await client?.send(.setGain(dB: on ? 0 : manualGainDb)) }
    }

    func setManualGain(_ db: Float) {
        manualGainDb = db
        if !autoGain {
            gainSendTask?.cancel()
            gainSendTask = Task { [weak self] in
                try? await Task.sleep(nanoseconds: Self.sliderDebounceNs)
                if Task.isCancelled { return }
                try? await self?.client?.send(.setGain(dB: db))
            }
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
        // hop. FFT is fed straight to SpectrumStore (lock-protected) so
        // the Metal renderer can snapshot it per frame; no MainActor hop
        // on the FFT hot path either, since the spectrum view doesn't go
        // through SwiftUI redraws.
        //
        // Non-audio events are dispatched to the MainActor as fire-and-
        // forget Tasks rather than awaited. Awaiting blocked the pump
        // whenever the main thread was busy, which made any main-thread
        // stall (heavy SwiftUI redraw, an unexpected system IPC, etc.)
        // also stall the audio pump — audio glitched in lockstep with
        // the UI. Submitted Tasks run on the MainActor in submission
        // order, so we don't reorder events; the worst case is a brief
        // backlog of pending Tasks if main stalls, which clears
        // automatically.
        let spectrum = self.spectrumStore
        for await event in await client.events {
            switch event {
            case .audio(let opus, let seq):
                holder.feed(opus, seq: seq)

            case .fft(let bins):
                spectrum.update(bins: bins)

            default:
                Task { @MainActor [weak self] in
                    self?.handle(event)
                }
            }
        }
        await MainActor.run {
            if self.state == .connected { self.state = .disconnected }
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
            // Only the meter changes on every frame; it lives on its own
            // observable object so this doesn't invalidate the whole view.
            meter.update(db: s)

            // Everything below is echoed back unchanged most of the time, so
            // guard each assignment: an unconditional write to a @Published
            // property publishes whether or not the value moved, and at 10 Hz
            // that is a pile of pointless SwiftUI invalidations.
            if q != squelchDb { squelchDb = q }

            // Captured before any mutation below: the old code read prevMode
            // after assigning mode, so it always compared equal and lock-screen
            // metadata never refreshed on a server-driven mode change.
            let prevFreq = freqHz
            let prevMode = mode

            if m != mode {
                mode = m
                let needRate = sampleRate(for: m)
                if needRate != sampleRate(for: prevMode) {
                    pipelineHolder.set(try? AudioPipeline(sampleRate: needRate))
                }
            }
            // Older Pi builds emit status without a freq field, decoded as 0.
            // Don't let that overwrite the optimistic local value the user
            // just set via tap-to-tune or the freq sheet.
            if f != 0, f != freqHz {
                freqHz = f
            }
            if let cc, cc != clientCount { clientCount = cc }
            // Only push lock-screen metadata when freq or mode actually
            // changed — calling MPNowPlayingInfoCenter on every status
            // frame was an XPC roundtrip per call and stalled the main
            // thread enough to freeze the UI + audio pump in lockstep,
            // with catch-up bursts every time the IPC unstuck.
            if freqHz != prevFreq || mode != prevMode {
                refreshNowPlaying()
            }

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
