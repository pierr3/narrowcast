import Foundation

// Messages the client sends to the server (or relay).
// Every message goes out as a single QUIC datagram.
public enum ClientMessage: Sendable {
    case hello(version: UInt8 = protoVersion)
    case auth(passwordHash: Data) // 32 bytes (SHA-256)
    case start
    case stop
    case setFrequency(hz: UInt64)
    case setMode(DemodMode)
    case setSquelch(dBm: Float)
    case setGain(dB: Float) // 0 = auto
    case qualityReport(audioLossPct: UInt8, fftLossPct: UInt8, windowMs: UInt16)

    public func encode() -> Data {
        var w = ByteWriter()
        switch self {
        case .hello(let v):
            w.u8(CommandType.hello.rawValue)
            w.u8(v)
        case .auth(let hash):
            w.u8(CommandType.auth.rawValue)
            w.bytes(hash)
        case .start:
            w.u8(CommandType.start.rawValue)
        case .stop:
            w.u8(CommandType.stop.rawValue)
        case .setFrequency(let hz):
            w.u8(CommandType.setFrequency.rawValue)
            w.u64LE(hz)
        case .setMode(let m):
            w.u8(CommandType.setMode.rawValue)
            w.u8(m.rawValue)
        case .setSquelch(let db):
            w.u8(CommandType.setSquelch.rawValue)
            w.f32LE(db)
        case .setGain(let db):
            w.u8(CommandType.setGain.rawValue)
            w.f32LE(db)
        case .qualityReport(let a, let f, let ms):
            w.u8(CommandType.qualityReport.rawValue)
            w.u8(a)
            w.u8(f)
            w.u16LE(ms)
        }
        return w.data
    }
}
