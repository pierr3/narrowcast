import Foundation
import NarrowcastProtocol

// NarrowcastClient is the high-level API consumed by the UI. It owns a
// QUICTransport, performs the auth + Hello handshake, parses inbound
// datagrams into typed Events, and exposes helpers for the commands the
// UI cares about (set freq, set mode, etc).
//
// Internally a single pump consumes transport.inbound — AsyncStream is
// iterable only once, so auth/welcome handshakes are completed by routing
// matched messages to one-shot continuations and forwarding the rest to
// the public events stream.
public actor NarrowcastClient {

    public struct ServerInfo: Sendable, Equatable {
        public let protocolVersion: UInt8
        public let minHz: UInt64
        public let maxHz: UInt64
        public let sampleRate: Float
    }

    public enum Event: Sendable {
        case welcome(ServerInfo)
        case audio(Data)
        case fft(bins: [UInt8])
        case status(smeter: Float, squelch: Float, mode: DemodMode, freq: UInt64, clientCount: UInt8?)
        case loss(LossTracker.Sample)
        case unknown(typeByte: UInt8)
        case disconnected
    }

    public enum ConnectError: Error, CustomStringConvertible {
        case authFailed
        case authTimeout
        case welcomeTimeout
        case transport(Error)

        public var description: String {
            switch self {
            case .authFailed:    return "Authentication failed"
            case .authTimeout:   return "Auth timed out — relay didn't reply"
            case .welcomeTimeout: return "Welcome timed out — server didn't reply"
            case .transport(let e): return "Transport: \(e)"
            }
        }
    }

    public struct Config: Sendable {
        public let host: String
        public let port: UInt16
        public let password: String?
        public let mode: QUICTransport.Mode

        public init(host: String, port: UInt16, password: String?, mode: QUICTransport.Mode) {
            self.host = host
            self.port = port
            self.password = password
            self.mode = mode
        }
    }

    private let transport: QUICTransport
    private let password: String?
    private let lossTracker = LossTracker()

    private var eventsContinuation: AsyncStream<Event>.Continuation?
    public let events: AsyncStream<Event>

    private var authWaiter: OneShot<Void>?
    private var welcomeWaiter: OneShot<ServerInfo>?
    private var pumpTask: Task<Void, Never>?

    public init(config: Config) {
        self.transport = QUICTransport(host: config.host, port: config.port, mode: config.mode)
        self.password = config.password
        var cont: AsyncStream<Event>.Continuation!
        self.events = AsyncStream { c in cont = c }
        self.eventsContinuation = cont
    }

    /// Connect, authenticate (if password set), send Hello, wait for Welcome.
    /// Starts the inbound pump immediately after the transport is ready so
    /// auth/welcome replies are observed via one-shot waiters routed by the
    /// pump (AsyncStream supports only one iterator).
    public func connect() async throws -> ServerInfo {
        do {
            try await transport.connect()
        } catch {
            throw ConnectError.transport(error)
        }

        // Pump must be running before we send anything that expects a reply
        // — otherwise the reply could land before we start consuming inbound.
        pumpTask = Task { [weak self] in
            await self?.runPump()
        }

        if let password {
            let waiter = OneShot<Void>(timeout: 5.0, onTimeout: ConnectError.authTimeout)
            self.authWaiter = waiter
            try await transport.send(ClientMessage.auth(passwordHash: PasswordHash.sha256(password)).encode())
            try await waiter.value
        }

        // Welcome is fanned out from the Pi via the relay; if the Pi was
        // bouncing or the first Hello was dropped before our client was in
        // the relay's fan-out map, we'd hang. Retry Hello a few times
        // before giving up — the Pi sends Welcome on every Hello received.
        let welcome = OneShot<ServerInfo>(timeout: 8.0, onTimeout: ConnectError.welcomeTimeout)
        self.welcomeWaiter = welcome
        try await transport.send(ClientMessage.hello().encode())

        // Background re-sender; cancels if Welcome arrives.
        let retry = Task { [weak self] in
            for _ in 0..<3 {
                try? await Task.sleep(nanoseconds: 1_500_000_000)
                if Task.isCancelled { return }
                guard let self else { return }
                if await self.welcomeWaiter == nil { return }
                NSLog("[narrowcast] no welcome — resending Hello")
                try? await self.transport.send(ClientMessage.hello().encode())
            }
        }

        do {
            let info = try await welcome.value
            retry.cancel()
            return info
        } catch {
            retry.cancel()
            throw error
        }
    }

    public func send(_ message: ClientMessage) async throws {
        try await transport.send(message.encode())
    }

    public func close() async {
        pumpTask?.cancel()
        pumpTask = nil
        await transport.close()
        eventsContinuation?.yield(.disconnected)
        eventsContinuation?.finish()
        eventsContinuation = nil
    }

    // MARK: - Pump

    private func runPump() async {
        for await datagram in transport.inbound {
            guard let msg = ServerMessage.decode(datagram) else {
                NSLog("[narrowcast] dropped undecodable datagram (\(datagram.count) B)")
                continue
            }
            switch msg {
            case .authOK:
                await authWaiter?.fulfill(.success(()))
                authWaiter = nil

            case .authFail:
                NSLog("[narrowcast] auth rejected by relay")
                await authWaiter?.fulfill(.failure(ConnectError.authFailed))
                authWaiter = nil

            case .welcome(let v, let lo, let hi, let sr):
                let info = ServerInfo(protocolVersion: v, minHz: lo, maxHz: hi, sampleRate: sr)
                await welcomeWaiter?.fulfill(.success(info))
                welcomeWaiter = nil
                eventsContinuation?.yield(.welcome(info))

            case .audio(let opus):
                lossTracker.recordReceived(.audio)
                eventsContinuation?.yield(.audio(opus))

            case .fft(let bins):
                lossTracker.recordReceived(.fft)
                eventsContinuation?.yield(.fft(bins: bins))

            case .status(let s, let q, let m, let f, let cc):
                lossTracker.recordReceived(.status)
                eventsContinuation?.yield(.status(smeter: s, squelch: q, mode: m, freq: f, clientCount: cc))

            case .seqMark(let a, let f, let s):
                if let sample = lossTracker.observeSeqMark(audioSent: a, fftSent: f, statusSent: s) {
                    eventsContinuation?.yield(.loss(sample))
                    // Echo the loss measurement back to the server so it can
                    // adapt FFT rate + Opus bitrate. Best-effort; failure to
                    // send is harmless — server falls back to full quality
                    // when no reports arrive.
                    let report = ClientMessage.qualityReport(
                        audioLossPct: sample.audioLossPct,
                        fftLossPct: sample.fftLossPct,
                        windowMs: sample.windowMs
                    )
                    try? await transport.send(report.encode())
                }

            case .unknown(let t):
                NSLog("[narrowcast] unknown datagram type 0x%02x", t)
                eventsContinuation?.yield(.unknown(typeByte: t))
            }
        }
        eventsContinuation?.yield(.disconnected)
        eventsContinuation?.finish()
        eventsContinuation = nil
    }
}

// OneShot is a single-fulfilment async waiter with a deadline. Used to bridge
// individual handshake replies into structured async/await control flow.
fileprivate actor OneShot<T: Sendable> {
    private var resolved: Result<T, Error>?
    private var continuation: CheckedContinuation<T, Error>?
    private let timeout: TimeInterval
    private let onTimeoutError: Error
    private var timeoutTask: Task<Void, Never>?

    init(timeout: TimeInterval, onTimeout: Error) {
        self.timeout = timeout
        self.onTimeoutError = onTimeout
    }

    var value: T {
        get async throws {
            if let resolved {
                switch resolved {
                case .success(let v): return v
                case .failure(let e): throw e
                }
            }
            return try await withCheckedThrowingContinuation { cc in
                self.continuation = cc
                self.timeoutTask = Task { [weak self] in
                    try? await Task.sleep(nanoseconds: UInt64((self?.timeout ?? 5) * 1_000_000_000))
                    await self?.fireTimeout()
                }
            }
        }
    }

    func fulfill(_ result: Result<T, Error>) {
        guard resolved == nil else { return }
        resolved = result
        timeoutTask?.cancel()
        timeoutTask = nil
        guard let cc = continuation else { return }
        continuation = nil
        switch result {
        case .success(let v): cc.resume(returning: v)
        case .failure(let e): cc.resume(throwing: e)
        }
    }

    private func fireTimeout() {
        guard resolved == nil else { return }
        resolved = .failure(onTimeoutError)
        guard let cc = continuation else { return }
        continuation = nil
        cc.resume(throwing: onTimeoutError)
    }
}
