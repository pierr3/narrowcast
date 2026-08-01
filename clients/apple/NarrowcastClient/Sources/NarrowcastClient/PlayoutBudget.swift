import Foundation

/// PlayoutBudget decides whether a freshly arrived audio packet should be
/// played or dropped, given how much audio is already queued ahead of it.
///
/// The engine drains the playout queue at the device clock and nothing else, so
/// queue depth *is* the playback latency — and it only moves one way on its own:
/// a stall adds packets that never come back out. Two different mechanisms are
/// needed to keep it bounded, because there are two different ways it grows.
///
///   * A burst past `maxFrames` is shed immediately, down to `targetFrames`,
///     with the gap between the two acting as hysteresis so one burst is shed
///     once instead of oscillating around the ceiling.
///
///   * Standing latency *below* that ceiling is shed as well. A ceiling on its
///     own bounds the worst case but leaves every backlog under it a stable
///     resting place: one burst — a network catch-up, the server draining IQ it
///     fell behind on, an uplink reconnect — parks the queue at 250 ms and
///     nothing brings it back, so the stream feels late for the rest of the
///     session even on a LAN with zero loss.
///
/// The low-water mark over a window is what tells those apart. Buffer that is
/// absorbing jitter gets consumed and refilled, so the minimum dips toward zero;
/// buffer that is pure standing latency is never touched at all. If the backlog
/// has not once been below target for a whole window, that excess is provably
/// unnecessary and can be shed without costing any resilience — which is why the
/// ceiling can stay generous for genuinely lossy links.
///
/// The excess goes in one step rather than being trimmed gradually: a single
/// ~150 ms cut is one glitch, where dropping a packet at a time would chop
/// repeatedly for as long as catching up took.
public struct PlayoutBudget: Sendable {

    /// Backlog ceiling before shedding starts.
    public let maxFrames: Int
    /// The level shedding drains down to, and the most standing latency
    /// tolerated indefinitely.
    public let targetFrames: Int
    /// How long the backlog must stay above target before its excess counts as
    /// standing latency. Long enough that ordinary jitter dips the minimum below
    /// target within the window; short enough not to leave a listener behind.
    public let window: Double

    private var shedding = false
    private var windowMinFrames = Int.max
    private var windowStart: Double = 0
    private var dropped = 0

    public init(maxFrames: Int, targetFrames: Int, window: Double = 5) {
        precondition(targetFrames < maxFrames, "target must leave room below the ceiling for hysteresis")
        self.maxFrames = maxFrames
        self.targetFrames = targetFrames
        self.window = window
    }

    /// Packets dropped to keep the backlog bounded. Diagnostics only.
    public var droppedPackets: Int { dropped }

    /// True while draining down to `targetFrames`.
    public var isShedding: Bool { shedding }

    /// Whether a packet arriving now should be played. `queuedFrames` is the
    /// backlog *ahead* of it, and `now` is a monotonic clock in seconds.
    public mutating func admit(queuedFrames: Int, now: Double) -> Bool {
        noteBacklog(queuedFrames, now: now)

        if shedding {
            if queuedFrames > targetFrames {
                dropped += 1
                return false
            }
            shedding = false
        } else if queuedFrames > maxFrames {
            shedding = true
            dropped += 1
            return false
        }
        return true
    }

    /// Forget accumulated history. For use when playback is torn down, so a
    /// stale window can't trigger a shed against a freshly started stream.
    public mutating func reset() {
        shedding = false
        windowMinFrames = .max
        windowStart = 0
    }

    private mutating func noteBacklog(_ queuedFrames: Int, now: Double) {
        if windowStart == 0 {
            windowStart = now
        }
        // Taken before the window check, so a queue that has just run dry is
        // credited to the window that is closing rather than the next one.
        windowMinFrames = min(windowMinFrames, queuedFrames)

        guard now - windowStart >= window else { return }
        let standing = windowMinFrames
        windowStart = now
        windowMinFrames = queuedFrames
        if standing > targetFrames {
            shedding = true
        }
    }
}
