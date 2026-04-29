import Foundation

// Server is a saved relay or direct-Pi entry. Password stored separately
// in the Keychain (Phase 10), never in this struct.
public struct Server: Identifiable, Codable, Equatable, Hashable {
    public var id: UUID
    public var name: String
    public var host: String
    public var port: UInt16
    public var requiresPassword: Bool
    public var allowSelfSigned: Bool

    public init(id: UUID = UUID(), name: String, host: String, port: UInt16 = 443,
                requiresPassword: Bool = true, allowSelfSigned: Bool = false) {
        self.id = id
        self.name = name
        self.host = host
        self.port = port
        self.requiresPassword = requiresPassword
        self.allowSelfSigned = allowSelfSigned
    }

    public var hostPort: String { "\(host):\(port)" }
}
