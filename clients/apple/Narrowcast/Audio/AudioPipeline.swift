import Foundation

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

    init(sampleRate: Int) throws {
        self.decoder = try OpusDecoder(sampleRate: sampleRate)
        // Player is created but NOT started — playback begins automatically
        // once the ring buffer crosses its preroll threshold (~60 ms). That
        // way the first render-callback tick draws from a non-empty ring
        // instead of zero-filling and gapping out.
        self.player = try AudioPlayer(sampleRate: sampleRate)
    }

    func feed(_ opus: Data) {
        queue.async { [decoder, player] in
            if let pcm = decoder.decode(opus) {
                player.enqueue(pcm)
            }
        }
    }

    func stop() {
        queue.sync {
            self.lastPacket = nil
        }
        player.stop()
    }
}
