import Foundation
import NarrowcastProtocol

// A saved freq + mode + squelch + gain bundle. One tap on a favorite chip
// dispatches all four commands to the server. Stored globally rather than
// per-server because a "144.800 NFM" preset is meaningful across any
// narrowcast deployment a user connects to.
public struct Favorite: Identifiable, Codable, Equatable, Hashable {
    public var id: UUID
    public var name: String
    public var freqHz: UInt64
    public var mode: DemodMode
    public var squelchDb: Float
    public var gainAuto: Bool
    public var gainDb: Float

    public init(id: UUID = UUID(),
                name: String,
                freqHz: UInt64,
                mode: DemodMode,
                squelchDb: Float,
                gainAuto: Bool,
                gainDb: Float) {
        self.id = id
        self.name = name
        self.freqHz = freqHz
        self.mode = mode
        self.squelchDb = squelchDb
        self.gainAuto = gainAuto
        self.gainDb = gainDb
    }

    public var freqMHz: Double { Double(freqHz) / 1_000_000 }
}
