import Foundation
import Network
import CryptoKit

// QUICTransport wraps Network.framework's QUIC datagram path. The documented
// pattern for unreliable QUIC datagrams (RFC 9221) uses NWConnectionGroup
// over an NWMultiplexGroup descriptor — NWConnection alone exposes streams
// only. The group's send/receive handlers operate on datagrams directly.
//
// Self-signed cert handling: the Pi runs a generated cert that no public CA
// trusts, so connecting directly to the Pi (vs through the relay's Let's
// Encrypt cert) requires bypassing default verification. We accept either
// "trust everything" (mode .acceptUnverified) or pinning by SHA-256 of the
// leaf certificate's DER bytes.
public actor QUICTransport {

    public enum Mode: Sendable {
        case verifyDefault                    // public CA, e.g. Let's Encrypt on the relay
        case acceptUnverified                 // self-signed Pi, no pinning (dev only)
        case pinFingerprint(_ sha256: Data)   // self-signed Pi, pinned by leaf cert SHA-256
    }

    public enum TransportError: Error {
        case notConnected
        case sendFailed(NWError)
        case connectionFailed(NWError)
        case cancelled
    }

    private let host: String
    private let port: UInt16
    private let mode: Mode
    private let alpn: String

    private var group: NWConnectionGroup?
    private var streamConnection: NWConnection?
    private var inboundContinuation: AsyncStream<Data>.Continuation?

    public let inbound: AsyncStream<Data>

    /// Inbound buffer depth. AsyncStream defaults to unbounded, which for a
    /// realtime datagram feed is a latency trap: any consumer stall (a busy main
    /// thread, a slow decode) silently grows the queue, and the backlog is then
    /// delivered as a burst instead of dropped. 64 datagrams is a little over a
    /// second of audio; past that, newest-wins is the correct behaviour on a
    /// path that already assumes loss.
    private static let inboundDepth = 64

    public init(host: String, port: UInt16, mode: Mode, alpn: String = "narrowcast-v1") {
        self.host = host
        self.port = port
        self.mode = mode
        self.alpn = alpn
        var cont: AsyncStream<Data>.Continuation!
        self.inbound = AsyncStream(bufferingPolicy: .bufferingNewest(Self.inboundDepth)) { c in cont = c }
        self.inboundContinuation = cont
    }

    public func connect() async throws {
        let quicOpts = NWProtocolQUIC.Options(alpn: [alpn])

        // RFC 9221 datagrams. isDatagram + maxDatagramFrameSize > 0 are both
        // needed; without either, group.send sends nothing on the wire.
        // (Reference: alta/swift-quic-datagram-example, Apple Forums 685976.)
        // 1220 B is safely under typical UDP MTU and what most QUIC stacks
        // negotiate by default; usableDatagramFrameSize from the post-handshake
        // metadata exposes the actual negotiated size.
        quicOpts.isDatagram = true
        quicOpts.maxDatagramFrameSize = 1220

        // Streams: even datagram-only NWConnectionGroups silently open one
        // bidirectional stream during handshake (RFC 9000 forces lower stream
        // IDs to open when higher ones are used). Setting these limits is
        // belt-and-braces — Apple's defaults work for our quic-go server but
        // explicit values protect against future server config changes.
        // (Reference: Apple Forums 744892, 739169 — hidden-stream behaviour.)
        quicOpts.direction = .bidirectional
        quicOpts.initialMaxStreamsBidirectional = 4
        quicOpts.initialMaxStreamsUnidirectional = 4

        // Connection-level flow-control window. 1 MiB is generous for the
        // datagram-only path (datagrams don't consume this) and stops
        // server-side flow control from starving us.
        quicOpts.initialMaxData = 1_048_576

        // 120 s idle. Keepalive pings (re-sent Hello every 15 s, see
        // NarrowcastClient) keep the link warm well within this window;
        // the 4× margin tolerates a brief mobile dead-zone or a sleeping
        // phone before the connection drops.
        quicOpts.idleTimeout = 120_000

        configureSecurity(quicOpts.securityProtocolOptions)

        let params = NWParameters(quic: quicOpts)
        let endpoint = NWEndpoint.hostPort(
            host: NWEndpoint.Host(host),
            port: NWEndpoint.Port(rawValue: port)!
        )

        let descriptor = NWMultiplexGroup(to: endpoint)
        let group = NWConnectionGroup(with: descriptor, using: params)
        self.group = group

        // Inbound datagrams arrive here. The simpler setReceiveHandler form
        // is what the working example uses; the one with explicit max size +
        // rejectOversized parameters appears not to be wired up for QUIC
        // datagram delivery.
        //
        // The continuation is captured directly instead of hopping through the
        // actor. `yield` is Sendable and synchronous, so this both avoids a Task
        // allocation per datagram (~90/s at default rates) and — more
        // importantly — preserves ordering: tasks enqueued on an actor have no
        // FIFO guarantee, so the old `Task { await deliverInbound(…) }` could
        // reorder Opus packets on their way to the decoder.
        let cont = self.inboundContinuation
        group.setReceiveHandler { _, content, _ in
            guard let content, !content.isEmpty else { return }
            cont?.yield(content)
        }

        try await withCheckedThrowingContinuation { (cc: CheckedContinuation<Void, Error>) in
            let resumeBox = ResumeOnce(cc)
            group.stateUpdateHandler = { [weak self] state in
                guard let self else { return }
                switch state {
                case .ready:
                    // Diagnostic: read the negotiated path MTU. quic-go
                    // typically settles around 1200; iCloud Private Relay
                    // can knock this lower. Logged once per connection.
                    if let meta = group.metadata(definition: NWProtocolQUIC.definition) as? NWProtocolQUIC.Metadata {
                        NSLog("[narrowcast] QUIC ready, usableDatagramFrameSize=%d", meta.usableDatagramFrameSize)
                    } else {
                        NSLog("[narrowcast] QUIC ready (no metadata)")
                    }
                    resumeBox.resume(.success(()))
                case .failed(let err):
                    NSLog("[narrowcast] connection failed: \(err)")
                    resumeBox.resume(.failure(TransportError.connectionFailed(err)))
                    Task { await self.handleDisconnect() }
                case .cancelled:
                    resumeBox.resume(.failure(TransportError.cancelled))
                    Task { await self.handleDisconnect() }
                case .waiting(let err):
                    NSLog("[narrowcast] connection waiting: \(err)")
                default:
                    break
                }
            }
            group.start(queue: .global(qos: .userInitiated))
        }
    }

    public func send(_ datagram: Data) async throws {
        guard let group = self.group else { throw TransportError.notConnected }
        try await withCheckedThrowingContinuation { (cc: CheckedContinuation<Void, Error>) in
            group.send(content: datagram) { err in
                if let err {
                    NSLog("[narrowcast] send error: \(err)")
                    cc.resume(throwing: TransportError.sendFailed(err))
                } else {
                    cc.resume()
                }
            }
        }
    }

    public func close() {
        streamConnection?.cancel()
        streamConnection = nil
        group?.cancel()
        group = nil
        inboundContinuation?.finish()
        inboundContinuation = nil
    }

    // MARK: - Private

    private func handleDisconnect() {
        inboundContinuation?.finish()
        inboundContinuation = nil
    }

    private func configureSecurity(_ opts: sec_protocol_options_t) {
        switch mode {
        case .verifyDefault:
            return
        case .acceptUnverified:
            sec_protocol_options_set_verify_block(opts, { _, _, completion in
                completion(true)
            }, .global(qos: .userInitiated))
        case .pinFingerprint(let pinned):
            sec_protocol_options_set_verify_block(opts, { _, secTrust, completion in
                let trust = sec_trust_copy_ref(secTrust).takeRetainedValue()
                guard let chain = SecTrustCopyCertificateChain(trust) as? [SecCertificate],
                      let leaf = chain.first else {
                    completion(false); return
                }
                let der = SecCertificateCopyData(leaf) as Data
                let digest = Data(SHA256.hash(data: der))
                completion(digest == pinned)
            }, .global(qos: .userInitiated))
        }
    }
}

// ResumeOnce guards a CheckedContinuation against double-resume from
// re-entrant state-update callbacks (e.g. .ready followed by .cancelled).
fileprivate final class ResumeOnce: @unchecked Sendable {
    private let lock = NSLock()
    private var continuation: CheckedContinuation<Void, Error>?

    init(_ cc: CheckedContinuation<Void, Error>) {
        self.continuation = cc
    }

    func resume(_ result: Result<Void, Error>) {
        lock.lock(); defer { lock.unlock() }
        guard let cc = continuation else { return }
        continuation = nil
        switch result {
        case .success: cc.resume()
        case .failure(let e): cc.resume(throwing: e)
        }
    }
}
