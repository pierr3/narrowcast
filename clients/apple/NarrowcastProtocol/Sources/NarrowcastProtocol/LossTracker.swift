import Foundation

// LossTracker turns the server's periodic SeqMark datagrams into a measured
// loss rate the client can report back via CmdQualityReport. The server emits
// monotonically increasing send-counts every ~1 s. Each time we observe one
// we diff (sent_now - sent_at_last_mark) against (received_now - received_at_last_mark)
// to derive the percent that went missing in the window.
//
// Wraparound: counters are u32. With audio at ~50 packets/s and fft at ~10/s,
// wraparound is hours away. We treat any negative diff as a counter reset and
// emit 0 % loss for that window — false-positive on reconnect is worse than
// missing one window.
public final class LossTracker: @unchecked Sendable {
    public struct Sample: Sendable, Equatable {
        public let audioLossPct: UInt8
        public let fftLossPct: UInt8
        public let windowMs: UInt16
    }

    private struct Counts {
        var audio: UInt32 = 0
        var fft: UInt32 = 0
        var status: UInt32 = 0
    }

    private let lock = NSLock()
    private var receivedSinceStart = Counts()
    private var lastSentSnapshot: Counts?
    private var lastReceivedSnapshot: Counts?
    private var lastObserved: Date?

    public init() {}

    public func recordReceived(_ type: DatagramType) {
        lock.lock(); defer { lock.unlock() }
        switch type {
        case .audio:    receivedSinceStart.audio &+= 1
        case .fft:      receivedSinceStart.fft &+= 1
        case .status:   receivedSinceStart.status &+= 1
        case .seqMark, .welcome, .authOK, .authFail:
            break
        }
    }

    /// Call when a SeqMark datagram arrives. Returns a Sample if a window has
    /// elapsed since the previous mark; nil on the first observation (no
    /// baseline to diff against yet).
    public func observeSeqMark(audioSent: UInt32, fftSent: UInt32, statusSent: UInt32, now: Date = Date()) -> Sample? {
        lock.lock(); defer { lock.unlock() }
        let received = receivedSinceStart
        let sent = Counts(audio: audioSent, fft: fftSent, status: statusSent)

        defer {
            lastSentSnapshot = sent
            lastReceivedSnapshot = received
            lastObserved = now
        }

        guard let prevSent = lastSentSnapshot,
              let prevReceived = lastReceivedSnapshot,
              let prevAt = lastObserved else {
            return nil
        }

        // Pipeline restart: server reset counters (Stop+Start). Discard window.
        if sent.audio < prevSent.audio || sent.fft < prevSent.fft || sent.status < prevSent.status {
            return Sample(audioLossPct: 0, fftLossPct: 0, windowMs: 0)
        }

        let dtMs = max(1, Int(now.timeIntervalSince(prevAt) * 1000))
        let windowMs = UInt16(min(dtMs, Int(UInt16.max)))

        return Sample(
            audioLossPct: pct(deltaSent: sent.audio - prevSent.audio,
                              deltaReceived: received.audio &- prevReceived.audio),
            fftLossPct:   pct(deltaSent: sent.fft - prevSent.fft,
                              deltaReceived: received.fft &- prevReceived.fft),
            windowMs: windowMs
        )
    }

    public func reset() {
        lock.lock(); defer { lock.unlock() }
        receivedSinceStart = Counts()
        lastSentSnapshot = nil
        lastReceivedSnapshot = nil
        lastObserved = nil
    }

    private func pct(deltaSent: UInt32, deltaReceived: UInt32) -> UInt8 {
        guard deltaSent > 0 else { return 0 }
        if deltaReceived >= deltaSent { return 0 }
        let lost = deltaSent - deltaReceived
        let p = (UInt64(lost) * 100) / UInt64(deltaSent)
        return UInt8(min(p, 100))
    }
}
