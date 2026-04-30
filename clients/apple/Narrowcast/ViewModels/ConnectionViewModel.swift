import Foundation
import Combine
import SwiftUI
import NarrowcastProtocol
import NarrowcastClient

// ConnectionViewModel owns a NarrowcastClient and fans out the inbound
// AsyncStream of Events into @Published properties the SwiftUI views bind to.
// Audio playback hook is a stub (Phase 4) — incoming Opus packets are
// counted, not yet decoded.
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
    @Published var lastLoss: LossTracker.Sample?
    @Published var audioPacketsReceived: Int = 0
    @Published var fftFrameLatest: [UInt8] = []
    @Published var waterfallFrames: [[UInt8]] = []   // newest at index 0
    private let waterfallDepth = 120

    private var client: NarrowcastClient?
    private var pump: Task<Void, Never>?
    private var decoder: OpusDecoder?
    private var audio: AudioPlayer?
    private var lastOpusPacket: Data?

    func connect(server: Server, password: String?) {
        guard state != .connecting && state != .connected else { return }

        // Relay always demands auth as the first datagram; sending Hello
        // first triggers an immediate close and looks like a network glitch
        // ("Network is down" / EINVAL) instead of an obvious misconfig.
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
                await self?.runEventPump(client: client)
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
        audio?.stop()
        audio = nil
        decoder = nil
        lastOpusPacket = nil
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
        freqHz = hz // optimistic; reconciled by next status frame
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

    func setGain(autoGain: Bool, db: Float) {
        Task { try? await client?.send(.setGain(dB: autoGain ? 0 : db)) }
    }

    private func runEventPump(client: NarrowcastClient) async {
        for await event in await client.events {
            await MainActor.run {
                self.handle(event)
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
        // Tear down any prior session on reconnect.
        audio?.stop()
        decoder = try? OpusDecoder(sampleRate: sampleRate)
        audio = try? AudioPlayer(sampleRate: sampleRate)
        try? audio?.start()
        lastOpusPacket = nil
    }

    /// Switch the decoder/player to a new sample rate (mode change WFM↔NFM/AM
    /// flips between 48 kHz and 16 kHz). Called from a status frame whose
    /// implied rate differs from the current one. Brief gap during swap is
    /// preferred to mismatched rates (resampling distortion).
    private func resetAudio(sampleRate: Int) {
        audio?.stop()
        decoder = try? OpusDecoder(sampleRate: sampleRate)
        audio = try? AudioPlayer(sampleRate: sampleRate)
        try? audio?.start()
        lastOpusPacket = nil
    }

    private func handle(_ event: NarrowcastClient.Event) {
        switch event {
        case .welcome(let info):
            serverInfo = info
        case .audio(let opus):
            audioPacketsReceived &+= 1
            if let pcm = decoder?.decode(opus) {
                audio?.enqueue(pcm)
            }
            lastOpusPacket = opus
        case .fft(let bins):
            fftFrameLatest = bins
            waterfallFrames.insert(bins, at: 0)
            if waterfallFrames.count > waterfallDepth {
                waterfallFrames.removeLast(waterfallFrames.count - waterfallDepth)
            }
        case .status(let s, let q, let m, let f, let cc):
            sMeterDb = s
            squelchDb = q
            // Mode change flips the audio sample rate (WFM=48k, NFM/AM=16k).
            // Rebuild the decoder + player when it shifts; brief gap is
            // preferable to playing 48k samples through a 16k pipeline.
            if m != mode {
                let prev = mode
                mode = m
                let needRate = sampleRate(for: m)
                let prevRate = sampleRate(for: prev)
                if needRate != prevRate {
                    resetAudio(sampleRate: needRate)
                }
            }
            freqHz = f
            if let cc { clientCount = cc }
        case .loss(let sample):
            lastLoss = sample
            // Phase 11 will round-trip this back as CmdQualityReport.
        case .unknown:
            break
        case .disconnected:
            state = .disconnected
            streaming = false
        }
    }
}
