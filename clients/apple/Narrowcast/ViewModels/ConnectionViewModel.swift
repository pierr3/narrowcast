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
        case connecting
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
    @Published var streaming: Bool = false
    @Published var autoGain: Bool = true
    @Published var manualGainDb: Float = 20
    @Published var lastLoss: LossTracker.Sample?

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
        guard state != .connecting && state != .connected else { return }

        // Relay always demands auth as the first datagram. Without a password
        // we'd ship Hello, the relay would close us with "unexpected", and
        // the failure surfaces as opaque "Network is down" / EINVAL.
        if server.requiresPassword && (password?.isEmpty ?? true) {
            state = .error("Password required for this server")
            return
        }

        state = .connecting

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
                let info = try await client.connect()
                await MainActor.run {
                    self?.serverInfo = info
                    self?.state = .connected
                    self?.bootAudio(sampleRate: 16000)
                }
                let holder = await MainActor.run { self?.pipelineHolder }
                guard let holder else { return }
                await self?.runEventPump(client: client, holder: holder)
            } catch NarrowcastClient.ConnectError.authFailed {
                await MainActor.run { self?.state = .authFailed }
            } catch {
                await MainActor.run { self?.state = .error(String(describing: error)) }
            }
        }
    }

    func disconnect() {
        pump?.cancel()
        pump = nil
        pipelineHolder.stop()
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

    func startStreaming() {
        guard let client else { return }
        Task {
            try? await client.send(.start)
            await MainActor.run { self.streaming = true }
        }
    }

    func stopStreaming() {
        guard let client else { return }
        Task {
            try? await client.send(.stop)
            await MainActor.run { self.streaming = false }
        }
    }

    func setFrequency(_ hz: UInt64) {
        freqHz = hz // optimistic; may be reconciled by a status frame
        Task { try? await client?.send(.setFrequency(hz: hz)) }
    }

    func setMode(_ m: DemodMode) {
        mode = m
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
