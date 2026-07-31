import Foundation
import os

// AudioPipelineHolder is a thread-safe wrapper around a swappable
// AudioPipeline reference. Solves the stale-reference bug where the event
// pump captured a pipeline at start time, but a later mode change rebuilt
// the pipeline and the pump kept feeding the dead one.
//
// The holder is sendable across the actor boundary; feed(_:) takes the
// lock just long enough to read the current pipeline pointer, then forwards
// off-lock so the audio queue can drain at full speed.
final class AudioPipelineHolder: @unchecked Sendable {
    private let lock = OSAllocatedUnfairLock<AudioPipeline?>(initialState: nil)

    func set(_ pipeline: AudioPipeline?) {
        let old = lock.withLock { state -> AudioPipeline? in
            let prev = state
            state = pipeline
            return prev
        }
        old?.stop()
    }

    func feed(_ opus: Data, seq: UInt16?) {
        let p = lock.withLock { $0 }
        p?.feed(opus, seq: seq)
    }

    func stop() {
        let old = lock.withLock { state -> AudioPipeline? in
            let prev = state
            state = nil
            return prev
        }
        old?.stop()
    }

    /// Playback backlog of the current pipeline, or nil if there isn't one.
    var latency: (seconds: Double, droppedPackets: Int)? {
        lock.withLock { $0 }?.latency
    }

    /// Pause local playback (keeps the decoder + engine attached, just
    /// stops feeding incoming Opus packets to the player).
    func pause() {
        lock.withLock { $0?.setMuted(true) }
    }

    func resume() {
        lock.withLock { $0?.setMuted(false) }
    }
}
