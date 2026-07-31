import Foundation
import Opus

// OpusDecoder wraps libopus's `OpusDecoder` C API for narrowband/wideband
// voice. Server's audio modes use 16 kHz mono (NFM/AM) or 48 kHz mono (WFM);
// we accept the rate at init and produce 32-bit float PCM.
//
// The encoder enables in-band FEC (see pkg/audio/opus.go on the server). On a
// dropped frame, calling `opus_decode_float` with the PREVIOUS packet's bytes
// and `decode_fec=1` reconstructs the missing frame from the FEC bits already
// inside it. Without this call, dropped packets are silent gaps; with it, the
// listener hears uninterrupted voice for losses up to ~10%.
public final class OpusDecoder {

    public enum DecodeError: Error {
        case createFailed(Int32)
    }

    private let decoder: OpaquePointer
    public let sampleRate: Int32
    public let frameSize: Int   // 20 ms at sampleRate

    public init(sampleRate: Int) throws {
        var err: Int32 = 0
        let dec = opus_decoder_create(Int32(sampleRate), 1, &err)
        guard err == OPUS_OK, let dec else {
            throw DecodeError.createFailed(err)
        }
        self.decoder = dec
        self.sampleRate = Int32(sampleRate)
        self.frameSize = sampleRate * 20 / 1000
    }

    deinit {
        opus_decoder_destroy(decoder)
    }

    /// Decode a packet to mono float PCM. Returns nil on libopus error.
    public func decode(_ packet: Data) -> [Float]? {
        var pcm = [Float](repeating: 0, count: frameSize * 2)
        let n = packet.withUnsafeBytes { (ptr: UnsafeRawBufferPointer) -> Int32 in
            guard let base = ptr.baseAddress?.assumingMemoryBound(to: UInt8.self) else { return -1 }
            return pcm.withUnsafeMutableBufferPointer { out in
                opus_decode_float(decoder, base, Int32(packet.count), out.baseAddress!, Int32(out.count), 0)
            }
        }
        guard n > 0 else { return nil }
        return Array(pcm.prefix(Int(n)))
    }

    /// Reconstruct a missing frame from the FEC payload of the packet that
    /// FOLLOWS it.
    ///
    /// Opus in-band FEC puts a low-bitrate copy of frame N inside packet N+1,
    /// so recovery needs the packet that arrived *after* the gap — pass the
    /// packet you just received, before decoding it normally. (The previous
    /// signature asked for the last packet received before the gap, which
    /// contains no copy of the missing frame.)
    public func decodeFEC(nextPacket: Data) -> [Float]? {
        var pcm = [Float](repeating: 0, count: frameSize)
        let n = nextPacket.withUnsafeBytes { (ptr: UnsafeRawBufferPointer) -> Int32 in
            guard let base = ptr.baseAddress?.assumingMemoryBound(to: UInt8.self) else { return -1 }
            return pcm.withUnsafeMutableBufferPointer { out in
                opus_decode_float(decoder, base, Int32(nextPacket.count), out.baseAddress!, Int32(out.count), 1)
            }
        }
        guard n > 0 else { return nil }
        return Array(pcm.prefix(Int(n)))
    }

    /// PLC: when no packet is available at all (not even a previous one for
    /// FEC), libopus synthesizes a short concealment frame. Pass nil bytes
    /// with `decode_fec=0`.
    public func decodePLC() -> [Float]? {
        var pcm = [Float](repeating: 0, count: frameSize)
        let n = pcm.withUnsafeMutableBufferPointer { out in
            opus_decode_float(decoder, nil, 0, out.baseAddress!, Int32(out.count), 0)
        }
        guard n > 0 else { return nil }
        return Array(pcm.prefix(Int(n)))
    }
}

private let OPUS_OK: Int32 = 0
