import Foundation
import NarrowcastProtocol

// NarrowcastClient is the high-level API consumed by the UI. It owns a
// QUICTransport, performs the auth + Hello handshake, parses inbound
// datagrams into typed Events, and exposes helpers for the commands the
// UI cares about (set freq, set mode, etc).
//
// Lifecycle: build with the server config, await `connect()` (which performs
// auth if a password is set, then sends Hello and waits for Welcome), then
// consume `events` and call command methods. Call `close()` when done.
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

    public enum ConnectError: Error {
        case authFailed
        case authTimeout
        case welcomeTimeout
        case transport(Error)
    }

    public struct Config: Sendable {
        public let host: String
        public let port: UInt16
        public let password: String?     // nil = direct Pi (no relay auth)
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

    private var serverInfo: ServerInfo?
    private var pumpTask: Task<Void, Never>?

    public init(config: Config) {
        self.transport = QUICTransport(host: config.host, port: config.port, mode: config.mode)
        self.password = config.password
        var cont: AsyncStream<Event>.Continuation!
        self.events = AsyncStream { c in cont = c }
        self.eventsContinuation = cont
    }

    /// Connect, authenticate (if password set), send Hello, wait for Welcome.
    /// On success the inbound pump is running; consume `events`.
    public func connect() async throws -> ServerInfo {
        do {
            try await transport.connect()
        } catch {
            throw ConnectError.transport(error)
        }

        if let password {
            try await performAuth(password: password)
        }

        try await transport.send(ClientMessage.hello().encode())

        // Pump runs until the transport closes. The first non-trivial frame
        // we expect is Welcome — surface it to the caller as the resolved
        // value of connect(), then keep pumping for the rest of the session.
        let welcomePromise = WelcomeWaiter()
        pumpTask = Task { [weak self] in
            await self?.runPump(welcomeWaiter: welcomePromise)
        }

        return try await welcomePromise.value
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

    // MARK: - Private

    private func performAuth(password: String) async throws {
        let hash = PasswordHash.sha256(password)
        try await transport.send(ClientMessage.auth(passwordHash: hash).encode())

        // Wait up to 5 s for AuthOK / AuthFail. Any other frame received in
        // this window is dropped — auth must be the first response.
        let deadline = Date().addingTimeInterval(5.0)
        for await datagram in transport.inbound {
            if Date() > deadline { break }
            guard let msg = ServerMessage.decode(datagram) else { continue }
            switch msg {
            case .authOK:
                return
            case .authFail:
                throw ConnectError.authFailed
            default:
                continue
            }
        }
        throw ConnectError.authTimeout
    }

    private func runPump(welcomeWaiter: WelcomeWaiter) async {
        for await datagram in transport.inbound {
            guard let msg = ServerMessage.decode(datagram) else { continue }
            switch msg {
            case .welcome(let v, let lo, let hi, let sr):
                let info = ServerInfo(protocolVersion: v, minHz: lo, maxHz: hi, sampleRate: sr)
                self.serverInfo = info
                await welcomeWaiter.fulfill(info)
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
                }

            case .authOK, .authFail:
                continue // already handled in performAuth phase

            case .unknown(let t):
                eventsContinuation?.yield(.unknown(typeByte: t))
            }
        }
        eventsContinuation?.yield(.disconnected)
    }
}

// Internal: a one-shot async waiter used to fulfill `connect()`'s return
// value when the first Welcome arrives.
fileprivate actor WelcomeWaiter {
    private var info: NarrowcastClient.ServerInfo?
    private var continuation: CheckedContinuation<NarrowcastClient.ServerInfo, Error>?

    var value: NarrowcastClient.ServerInfo {
        get async throws {
            if let info { return info }
            return try await withCheckedThrowingContinuation { cc in
                self.continuation = cc
                Task { await self.startTimeout() }
            }
        }
    }

    func fulfill(_ info: NarrowcastClient.ServerInfo) {
        guard self.info == nil else { return }
        self.info = info
        continuation?.resume(returning: info)
        continuation = nil
    }

    private func startTimeout() async {
        try? await Task.sleep(nanoseconds: 5_000_000_000)
        if info == nil {
            continuation?.resume(throwing: NarrowcastClient.ConnectError.welcomeTimeout)
            continuation = nil
        }
    }
}
