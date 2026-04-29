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

    private var client: NarrowcastClient?
    private var pump: Task<Void, Never>?

    func connect(server: Server, password: String?) {
        guard state != .connecting && state != .connected else { return }
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

    private func handle(_ event: NarrowcastClient.Event) {
        switch event {
        case .welcome(let info):
            serverInfo = info
        case .audio:
            // Decoder + AVAudioEngine playback land with the libopus xcframework
            // (Phase 4 task: vendor opus). For now we just count packets so the
            // UI shows the audio path is live.
            audioPacketsReceived &+= 1
        case .fft(let bins):
            fftFrameLatest = bins
        case .status(let s, let q, let m, let f, let cc):
            sMeterDb = s
            squelchDb = q
            mode = m
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
