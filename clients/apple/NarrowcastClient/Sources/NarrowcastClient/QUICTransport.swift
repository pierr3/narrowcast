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

    public init(host: String, port: UInt16, mode: Mode, alpn: String = "narrowcast-v1") {
        self.host = host
        self.port = port
        self.mode = mode
        self.alpn = alpn
        var cont: AsyncStream<Data>.Continuation!
        self.inbound = AsyncStream { c in cont = c }
        self.inboundContinuation = cont
    }

    public func connect() async throws {
        let quicOpts = NWProtocolQUIC.Options(alpn: [alpn])
        // Two flags are needed for RFC 9221 datagrams over Network.framework
        // QUIC. Without `isDatagram = true` the connection establishes but
        // group.send transmits no datagram frames — the server sees nothing.
        // (Reference: alta/swift-quic-datagram-example.) 1220 bytes is safely
        // under typical UDP MTU and what most QUIC stacks negotiate by default.
        quicOpts.isDatagram = true
        quicOpts.maxDatagramFrameSize = 1220
        quicOpts.idleTimeout = 30_000

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
        group.setReceiveHandler { [weak self] _, content, _ in
            guard let self else { return }
            guard let content, !content.isEmpty else { return }
            Task { await self.deliverInbound(content) }
        }

        try await withCheckedThrowingContinuation { (cc: CheckedContinuation<Void, Error>) in
            let resumeBox = ResumeOnce(cc)
            group.stateUpdateHandler = { [weak self] state in
                guard let self else { return }
                switch state {
                case .ready:
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

    private func deliverInbound(_ data: Data) {
        inboundContinuation?.yield(data)
    }

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
