import Foundation

// Messages received from the server (or via the relay). Each arrives as a
// single QUIC datagram. The relay appends a clientCount byte to Status frames
// before fan-out, so the same type may arrive in two lengths — we tolerate
// both.
public enum ServerMessage: Sendable {
    /// Raw Opus packet. `seq` is present only for DatagramAudioSeq (0x05);
    /// a nil seq means the server is an older build that can't be gap-detected,
    /// so the decoder falls back to plain decoding without FEC recovery.
    case audio(opus: Data, seq: UInt16?)
    case fft(bins: [UInt8])
    case status(smeter: Float, squelch: Float, mode: DemodMode, freq: UInt64, clientCount: UInt8?)
    case seqMark(audioSent: UInt32, fftSent: UInt32, statusSent: UInt32)
    case welcome(version: UInt8, minHz: UInt64, maxHz: UInt64, sampleRate: Float)
    case authOK
    case authFail
    case unknown(typeByte: UInt8) // forward-compatible fallback

    public static func decode(_ data: Data) -> ServerMessage? {
        guard !data.isEmpty else { return nil }
        var r = ByteReader(data: data)
        guard let typeByte = try? r.u8() else { return nil }

        switch typeByte {
        case DatagramType.audio.rawValue:
            return .audio(opus: r.remainingBytes(), seq: nil)

        case DatagramType.audioSeq.rawValue:
            // [u16le seq][opus...]
            guard let seq = try? r.u16LE() else { return nil }
            return .audio(opus: r.remainingBytes(), seq: seq)

        case DatagramType.fft.rawValue:
            // [u16be numBins][u8 bins...]
            guard let numBins = try? r.u16BE() else { return nil }
            guard let raw = try? r.bytes(Int(numBins)) else { return nil }
            return .fft(bins: Array(raw))

        case DatagramType.status.rawValue:
            // Current: [f32 smeter][f32 squelch][u8 mode][u64 freq] (+ optional u8 clientCount)
            // Older Pi builds (pre-2c25906) emit only [f32 smeter][f32 squelch][u8 mode]
            // (10 B + relay-appended client count = 11 B). Tolerate both so a
            // not-yet-redeployed Pi still drives the S-meter and mode pickup.
            guard let s = try? r.f32LE() else { return nil }
            guard let q = try? r.f32LE() else { return nil }
            guard let mb = try? r.u8() else { return nil }
            let mode = DemodMode(rawValue: mb) ?? .nfm
            let f: UInt64 = (try? r.u64LE()) ?? 0
            let cc: UInt8? = (try? r.u8())
            return .status(smeter: s, squelch: q, mode: mode, freq: f, clientCount: cc)

        case DatagramType.seqMark.rawValue:
            guard let a = try? r.u32LE() else { return nil }
            guard let f = try? r.u32LE() else { return nil }
            guard let s = try? r.u32LE() else { return nil }
            return .seqMark(audioSent: a, fftSent: f, statusSent: s)

        case DatagramType.welcome.rawValue:
            guard let v = try? r.u8() else { return nil }
            guard let lo = try? r.u64LE() else { return nil }
            guard let hi = try? r.u64LE() else { return nil }
            guard let sr = try? r.f32LE() else { return nil }
            return .welcome(version: v, minHz: lo, maxHz: hi, sampleRate: sr)

        case DatagramType.authOK.rawValue:
            return .authOK

        case DatagramType.authFail.rawValue:
            return .authFail

        default:
            return .unknown(typeByte: typeByte)
        }
    }
}
