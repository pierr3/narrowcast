import Foundation
import os

// AudioPipeline owns the OpusDecoder + AudioPlayer pair and runs the decode
// stage on a dedicated serial queue. The view model hands raw Opus packets
// to `feed(_:seq:)` from any thread; nothing blocks the caller.
//
// Two reasons this is its own class rather than methods on ConnectionViewModel:
//   1. The view model is @MainActor; doing Opus decode there pegs the
//      SwiftUI runloop at ~50 packets/sec.
//   2. Decoder + player are referenced by the audio queue closure. Putting
//      them in a non-actor class lets us capture them as stable Sendable
//      references without an await round-trip per packet.
final class AudioPipeline: @unchecked Sendable {
    private let decoder: OpusDecoder
    private let player: AudioPlayer
    private let queue = DispatchQueue(label: "narrowcast.audio", qos: .userInitiated)
    private let mutedFlag = OSAllocatedUnfairLock<Bool>(initialState: false)

    /// Next sequence number expected. Touched only on `queue`.
    private var expectedSeq: UInt16?

    /// How many consecutive missing frames are worth concealing. One comes back
    /// nearly intact from the FEC copy inside the next packet; past a few,
    /// synthesized concealment sounds worse than the gap it replaces, and a long
    /// "gap" is usually the server having stopped sending (squelch closed)
    /// rather than loss.
    private static let maxConcealedFrames = 3

    init(sampleRate: Int) throws {
        self.decoder = try OpusDecoder(sampleRate: sampleRate)
        self.player = try AudioPlayer(sampleRate: sampleRate)
    }

    /// Hand a received Opus packet to the decoder. `seq` comes from
    /// DatagramAudioSeq; nil means an older server that can't be gap-detected,
    /// in which case losses stay silent holes.
    func feed(_ opus: Data, seq: UInt16?) {
        if mutedFlag.withLock({ $0 }) { return }  // client-side pause
        queue.async { [self] in
            conceal(before: seq, using: opus)
            if let pcm = decoder.decode(opus) {
                player.enqueue(pcm)
            }
            if let seq { expectedSeq = seq &+ 1 }
        }
    }

    /// Fill in frames missing before `seq`, redeeming the in-band FEC the
    /// encoder already spends ~20-25 % of its bitrate on. Must run before the
    /// current packet is decoded, since libopus reconstructs the frame that
    /// preceded a packet from the FEC copy carried inside it.
    private func conceal(before seq: UInt16?, using opus: Data) {
        guard let seq, let expected = expectedSeq else { return }

        // Unsigned difference, so the u16 wrap (~22 min at 50 frames/s) is just
        // another small gap rather than a 65 000-frame hole.
        let gap = Int(seq &- expected)
        if gap == 0 { return }                     // in order
        if gap > Int(UInt16.max / 2) { return }    // stale reordered packet
        guard gap <= Self.maxConcealedFrames else { return }

        // Frames [expected, seq) are missing. Only the one immediately before
        // seq is carried in this packet's FEC; anything earlier gets libopus
        // packet-loss concealment.
        for _ in 0..<(gap - 1) {
            if let pcm = decoder.decodePLC() {
                player.enqueue(pcm)
            }
        }
        if let pcm = decoder.decodeFEC(nextPacket: opus) {
            player.enqueue(pcm)
        }
    }

    func setMuted(_ muted: Bool) {
        mutedFlag.withLock { $0 = muted }
        if muted { player.stop() }  // engine.stop() drains and silences
    }

    func stop() {
        queue.sync {
            self.expectedSeq = nil
        }
        player.stop()
    }
}
