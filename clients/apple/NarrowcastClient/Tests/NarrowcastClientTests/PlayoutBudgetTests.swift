import XCTest
@testable import NarrowcastClient

// 16 kHz mono, 20 ms packets — the AM/NFM case.
private let rate = 16000
private let packet = 320
private func frames(ms: Int) -> Int { rate * ms / 1000 }

private func newBudget(window: Double = 5) -> PlayoutBudget {
    PlayoutBudget(maxFrames: frames(ms: 400), targetFrames: frames(ms: 120), window: window)
}

final class PlayoutBudgetTests: XCTestCase {

    func testAdmitsWhileBacklogIsLow() {
        var b = newBudget()
        var t = 0.0
        for _ in 0..<100 {
            XCTAssertTrue(b.admit(queuedFrames: frames(ms: 60), now: t))
            t += 0.02
        }
        XCTAssertEqual(b.droppedPackets, 0)
    }

    // A burst past the ceiling shed down to target, not to zero: the hysteresis
    // gap is what stops it oscillating around the ceiling.
    func testShedsBurstDownToTargetThenResumes() {
        var b = newBudget()
        XCTAssertFalse(b.admit(queuedFrames: frames(ms: 450), now: 0))
        XCTAssertTrue(b.isShedding)

        // Still above target → keeps dropping.
        XCTAssertFalse(b.admit(queuedFrames: frames(ms: 300), now: 0.02))
        XCTAssertFalse(b.admit(queuedFrames: frames(ms: 150), now: 0.04))
        // At target → resumes.
        XCTAssertTrue(b.admit(queuedFrames: frames(ms: 110), now: 0.06))
        XCTAssertFalse(b.isShedding)
        XCTAssertEqual(b.droppedPackets, 3)
    }

    // The regression this type exists for: a backlog parked below the ceiling.
    // 260 ms is under the 400 ms ceiling, so the old shed-only-at-max rule left
    // it there forever.
    func testShedsStandingLatencyBelowTheCeiling() {
        var b = newBudget()
        var t = 0.0
        var admitted = 0
        // Held at 260 ms for two windows' worth of packets.
        for _ in 0..<600 {
            if b.admit(queuedFrames: frames(ms: 260), now: t) { admitted += 1 }
            t += 0.02
        }
        XCTAssertGreaterThan(b.droppedPackets, 0,
                             "260 ms of untouched backlog is standing latency and must be shed")
        XCTAssertGreaterThan(admitted, 0, "shedding must not silence the stream outright")
    }

    // The other half: a buffer that jitter is genuinely using must be left alone,
    // or a lossy link gets chopped for no reason.
    func testKeepsBufferThatJitterActuallyUses() {
        var b = newBudget()
        var t = 0.0
        // Backlog swings between nearly dry and 300 ms — deep, but working.
        let swing = [20, 120, 300, 200, 40, 260, 90, 10]
        for i in 0..<800 {
            XCTAssertTrue(b.admit(queuedFrames: frames(ms: swing[i % swing.count]), now: t),
                          "a working jitter buffer must not be shed")
            t += 0.02
        }
        XCTAssertEqual(b.droppedPackets, 0)
    }

    // A queue that drains at the end of every transmission is healthy by
    // definition, however deep it got mid-transmission.
    func testTransmissionThatDrainsIsNeverShed() {
        var b = newBudget()
        var t = 0.0
        for _ in 0..<20 {                        // 20 transmissions
            for ms in [0, 60, 140, 220, 180, 80] {
                XCTAssertTrue(b.admit(queuedFrames: frames(ms: ms), now: t))
                t += 0.02
            }
            t += 3.0                             // squelch closed, queue dry
        }
        XCTAssertEqual(b.droppedPackets, 0)
    }

    // Standing latency exactly at target is acceptable — shedding it would just
    // churn.
    func testDoesNotShedAtExactlyTarget() {
        var b = newBudget()
        var t = 0.0
        for _ in 0..<600 {
            XCTAssertTrue(b.admit(queuedFrames: frames(ms: 120), now: t))
            t += 0.02
        }
        XCTAssertEqual(b.droppedPackets, 0)
    }

    // A long silence must not make the first packet of the next transmission
    // look like it ended a window of standing latency.
    func testLongGapDoesNotShedOnResume() {
        var b = newBudget()
        // A transmission ending with a deep backlog.
        for i in 0..<10 {
            _ = b.admit(queuedFrames: frames(ms: 200), now: Double(i) * 0.02)
        }
        // 30 s later, queue long since dry.
        XCTAssertTrue(b.admit(queuedFrames: 0, now: 30))
        XCTAssertFalse(b.isShedding)
    }

    func testResetClearsSheddingAndWindow() {
        var b = newBudget()
        XCTAssertFalse(b.admit(queuedFrames: frames(ms: 450), now: 0))
        XCTAssertTrue(b.isShedding)
        b.reset()
        XCTAssertFalse(b.isShedding)
        XCTAssertTrue(b.admit(queuedFrames: frames(ms: 200), now: 0.02))
    }

    // Sanity: a packet's worth of audio is far below target, so ordinary
    // packet-by-packet arrival never trips anything.
    func testSinglePacketBacklogIsUncontroversial() {
        var b = newBudget()
        var t = 0.0
        for _ in 0..<1000 {
            XCTAssertTrue(b.admit(queuedFrames: packet, now: t))
            t += 0.02
        }
        XCTAssertEqual(b.droppedPackets, 0)
    }
}
