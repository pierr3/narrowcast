import Foundation
import QuartzCore

// SpectrumStore holds the latest FFT bins from the server, smoothed, peak-held
// and auto-ranged for display. Written from the inbound pump (background) and
// read from the Metal renderer (off-main) without crossing the main actor.
//
// Everything stays in the server's byte domain (0...255 spanning -120...0 dBFS,
// so ~0.47 dB per count) and is only normalized at read time. That is what lets
// the display window follow the signal instead of the absolute scale:
//
//   - Per-bin EMA absorbs frame-to-frame jitter while preserving spike shape.
//   - Per-bin peak hold with linear time decay, so peaks linger about a second.
//   - A smoothed floor/ceiling pair derived from each frame's percentiles
//     defines the 0...1 output range. Without it a quiet band renders as a row
//     of identical mid-height stubs, because real signals occupy maybe 30 dB of
//     a 120 dB scale.
//   - Aggregation into display bars happens here too (max per group), so the
//     renderer never touches individual bins.
//
// The lock is an NSLock rather than OSAllocatedUnfairLock because the read path
// hands buffers back to the caller to fill: OSAllocatedUnfairLock requires a
// @Sendable body, which can't touch caller-owned storage. Contention here is a
// writer at ≤10 Hz against a renderer that draws on demand, so the difference
// doesn't matter.
final class SpectrumStore: @unchecked Sendable {

    /// Install a callback invoked off-main whenever a frame lands, so a renderer
    /// can redraw on demand instead of spinning at display rate. Pass nil to
    /// detach (the renderer does this when it leaves the window hierarchy).
    func setFrameHandler(_ handler: (@Sendable () -> Void)?) {
        lock.lock()
        frameHandler = handler
        lock.unlock()
    }

    /// EMA blend factor for incoming frames. Higher = more responsive,
    /// lower = smoother but laggier.
    private let alpha: Float = 0.4

    /// Peak fall-off in counts per second. 60 counts ≈ 28 dB/s, so a peak
    /// crosses a typical display window in about a second.
    private let peakDecayPerSec: Float = 60

    /// How fast the auto-range window follows the signal. Deliberately slow: the
    /// window should drift with the noise floor, not breathe with speech.
    private let rangeFollowPerSec: Float = 2.5

    /// Narrowest display window in counts. ~30 dB stops a quiet band from being
    /// amplified into full-scale noise.
    private let minSpan: Float = 64

    private let lock = NSLock()
    private var frameHandler: (@Sendable () -> Void)?
    private var smooth: [Float] = []   // per-bin EMA, 0...255
    private var peak: [Float] = []     // per-bin peak hold, 0...255
    private var floor: Float = 0       // smoothed display floor, 0...255
    private var ceiling: Float = 255   // smoothed display ceiling, 0...255
    private var seeded = false
    private var lastDecay: CFTimeInterval = CACurrentMediaTime()
    private var histogram = [Int](repeating: 0, count: 256)

    /// Push a fresh FFT frame (0...255 from the server, already DC-centered
    /// because the server FFT-shifts).
    func update(bins: [UInt8]) {
        lock.lock()
        let n = bins.count
        if smooth.count != n {
            smooth = Array(repeating: 0, count: n)
            peak = Array(repeating: 0, count: n)
            seeded = false
        }
        for i in 0..<n {
            let v = Float(bins[i])
            smooth[i] = alpha * v + (1 - alpha) * smooth[i]
            if smooth[i] > peak[i] {
                peak[i] = smooth[i]
            }
        }
        updateRange(bins: bins)
        let handler = frameHandler
        lock.unlock()

        // Outside the lock: the handler is free to call back in.
        handler?()
    }

    /// Fill caller-owned buffers with normalized bar levels and peaks. Both
    /// arrays must be the same length, and that length is the bar count.
    /// Returns false if no frame has arrived yet.
    ///
    /// Aggregation uses max, not mean: a narrow carrier has to survive into its
    /// bar instead of being averaged into the noise around it.
    func readBars(level: inout [Float], peak barPeak: inout [Float]) -> Bool {
        precondition(level.count == barPeak.count, "level and peak must be the same length")
        let bars = level.count
        guard bars > 0 else { return false }

        lock.lock()
        defer { lock.unlock() }

        let n = smooth.count
        guard n > 0 else { return false }

        decayPeaks()

        let invSpan = 1 / max(ceiling - floor, minSpan)

        for b in 0..<bars {
            // Integer-exact group bounds, so every bin lands in exactly one bar.
            let start = b * n / bars
            var end = (b + 1) * n / bars
            if end <= start { end = start + 1 }

            var maxSmooth: Float = 0
            var maxPeak: Float = 0
            for i in start..<min(end, n) {
                maxSmooth = max(maxSmooth, smooth[i])
                maxPeak = max(maxPeak, peak[i])
            }
            level[b] = min(max((maxSmooth - floor) * invSpan, 0), 1)
            barPeak[b] = min(max((maxPeak - floor) * invSpan, 0), 1)
        }
        return true
    }

    func reset() {
        lock.lock()
        smooth.removeAll(keepingCapacity: true)
        peak.removeAll(keepingCapacity: true)
        seeded = false
        lock.unlock()
    }

    // MARK: - Private (all callers hold the lock)

    /// Track the display window from this frame's percentiles. Bins are bytes,
    /// so a 256-bucket histogram yields exact percentiles in one pass with no
    /// sorting; the buckets are reused across frames.
    private func updateRange(bins: [UInt8]) {
        for i in 0..<histogram.count { histogram[i] = 0 }
        for b in bins { histogram[Int(b)] += 1 }

        // The 20th percentile stays inside the noise floor even when a strong
        // signal occupies part of the span; the 99th tracks the loudest real
        // content without chasing one hot bin.
        let floorTarget = Float(percentile(count: bins.count, fraction: 0.20))
        let peakTarget = Float(percentile(count: bins.count, fraction: 0.99))
        let ceilingTarget = max(peakTarget, floorTarget + minSpan)

        if !seeded {
            floor = floorTarget
            ceiling = ceilingTarget
            seeded = true
            return
        }

        // Reuse the peak-decay clock for the follow rate; both want "time since
        // the display last advanced".
        let dt = Float(min(max(CACurrentMediaTime() - lastDecay, 0), 1))
        let k = min(rangeFollowPerSec * dt, 1)
        floor += (floorTarget - floor) * k
        ceiling += (ceilingTarget - ceiling) * k
    }

    private func percentile(count: Int, fraction: Float) -> Int {
        guard count > 0 else { return 0 }
        let threshold = Int(Float(count) * fraction)
        var running = 0
        for value in 0..<histogram.count {
            running += histogram[value]
            if running > threshold { return value }
        }
        return histogram.count - 1
    }

    /// Peak decay runs on read so it tracks wall-clock regardless of how often
    /// the server sends data (FFT drops to 1 fps under loss).
    private func decayPeaks() {
        let now = CACurrentMediaTime()
        let dt = Float(min(max(now - lastDecay, 0), 1))
        lastDecay = now
        let drop = peakDecayPerSec * dt
        guard drop > 0 else { return }
        for i in 0..<peak.count {
            peak[i] = max(smooth[i], peak[i] - drop)
        }
    }
}
