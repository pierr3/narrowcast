import Foundation
import QuartzCore
import os

// SpectrumStore holds the latest FFT bins from the server, smoothed and
// peak-tracked for display. Designed to be written from the inbound pump
// (background) and read from the Metal render thread (off-main) without
// crossing the main actor.
//
// Smoothing strategy:
//   - Per-bin EMA (alpha 0.4) absorbs jitter while preserving spike shape.
//   - Per-bin peak hold with linear time-decay so peaks linger ~1.5 s.
//
// All state lives behind a single OSAllocatedUnfairLock so the renderer
// can take a fast atomic snapshot once per frame.
final class SpectrumStore: @unchecked Sendable {

    struct Snapshot: Sendable {
        let smooth: [Float]   // 0...1
        let peak: [Float]     // 0...1
        let binCount: Int
    }

    private struct State {
        var smooth: [Float] = []
        var peak: [Float] = []
        var lastDecayTime: CFTimeInterval = CACurrentMediaTime()
    }

    private let state = OSAllocatedUnfairLock<State>(initialState: State())

    /// EMA blend factor for incoming frames. Higher = more responsive,
    /// lower = smoother but laggier.
    private let alpha: Float = 0.4

    /// Peak fall-off in normalized units per second. 0.7/s = a peak at the
    /// top falls to zero in ~1.4 s.
    private let peakDecayPerSec: Float = 0.7

    /// Push a fresh FFT frame (bytes 0...255 from the server). Bins are
    /// already DC-centered (server FFT-shifts).
    func update(bins: [UInt8]) {
        state.withLock { s in
            let n = bins.count
            if s.smooth.count != n {
                s.smooth = Array(repeating: 0, count: n)
                s.peak = Array(repeating: 0, count: n)
            }
            let inv: Float = 1.0 / 255.0
            for i in 0..<n {
                let v = Float(bins[i]) * inv
                s.smooth[i] = alpha * v + (1 - alpha) * s.smooth[i]
                if s.smooth[i] > s.peak[i] {
                    s.peak[i] = s.smooth[i]
                }
            }
        }
    }

    /// Renderer pulls a snapshot per frame. Decay applied here so peak
    /// hold falls at wall-clock rate regardless of how often the server
    /// sends data (FFT can drop to 1 fps under loss).
    func snapshot() -> Snapshot {
        state.withLock { s in
            let now = CACurrentMediaTime()
            let dt = Float(max(0, now - s.lastDecayTime))
            s.lastDecayTime = now
            let drop = peakDecayPerSec * dt
            if drop > 0 {
                for i in 0..<s.peak.count {
                    let target = max(s.smooth[i], s.peak[i] - drop)
                    s.peak[i] = target
                }
            }
            return Snapshot(
                smooth: s.smooth,
                peak: s.peak,
                binCount: s.smooth.count
            )
        }
    }

    func reset() {
        state.withLock { s in
            s.smooth.removeAll(keepingCapacity: true)
            s.peak.removeAll(keepingCapacity: true)
        }
    }
}
