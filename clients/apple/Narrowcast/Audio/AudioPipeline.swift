import Foundation
import os

// AudioPipeline owns the OpusDecoder + AudioPlayer pair and runs the decode
// stage on a dedicated serial queue. The view model hands raw Opus packets
// to `feed(_:)` from any thread; nothing blocks the caller.
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
    private var lastPacket: Data?  // touched only on `queue`; for FEC if we later wire it
    private let mutedFlag = OSAllocatedUnfairLock<Bool>(initialState: false)

    init(sampleRate: Int) throws {
        self.decoder = try OpusDecoder(sampleRate: sampleRate)
        self.player = try AudioPlayer(sampleRate: sampleRate)
    }

    func feed(_ opus: Data) {
        let muted = mutedFlag.withLock { $0 }
        if muted { return }  // client-side pause: drop incoming audio
        queue.async { [decoder, player] in
            if let pcm = decoder.decode(opus) {
                player.enqueue(pcm)
            }
        }
    }

    func setMuted(_ muted: Bool) {
        mutedFlag.withLock { $0 = muted }
        if muted { player.stop() }  // engine.stop() drains and silences
    }

    func stop() {
        queue.sync {
            self.lastPacket = nil
        }
        player.stop()
    }
}
