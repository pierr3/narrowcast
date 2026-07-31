import Foundation

public enum DatagramType: UInt8 {
    case audio = 0x01
    case fft = 0x02
    case status = 0x03
    case seqMark = 0x04
    /// Opus frame prefixed with a u16 sequence number. Same payload as `audio`
    /// otherwise; the counter is what lets the decoder tell a lost packet from
    /// a silent channel and spend the encoder's in-band FEC.
    case audioSeq = 0x05
    /// Echo of a ping token, for round-trip timing.
    case pong = 0x06
    case welcome = 0x31
    case authOK = 0x33
    case authFail = 0x34
}

public enum CommandType: UInt8 {
    case setFrequency = 0x10
    case setMode = 0x11
    case setSquelch = 0x12
    case setGain = 0x13
    case qualityReport = 0x14
    case ping = 0x15
    case start = 0x20
    case stop = 0x21
    case hello = 0x30
    case auth = 0x32
    case uplink = 0x35
}

public enum DemodMode: UInt8, CaseIterable, Sendable, Codable {
    case nfm = 0
    case wfm = 1
    case am = 2

    public var label: String {
        switch self {
        case .nfm: return "NFM"
        case .wfm: return "WFM"
        case .am: return "AM"
        }
    }
}

public let protoVersion: UInt8 = 1
