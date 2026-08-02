import XCTest
@testable import NarrowcastClient

// These two types sit on per-packet / per-frame paths, so their cost is worth a
// number rather than an assurance. Audio packets arrive ~50/s and status frames
// ~10/s, so anything in the nanoseconds is free.
final class HotPathCostTests: XCTestCase {

    func testPlayoutBudgetAdmitCost() {
        var b = PlayoutBudget(maxFrames: 6400, targetFrames: 1920)
        let iterations = 200_000
        let start = Date()
        var admitted = 0
        for i in 0..<iterations {
            if b.admit(queuedFrames: 1600, now: Double(i) * 0.02) { admitted += 1 }
        }
        let ns = Date().timeIntervalSince(start) / Double(iterations) * 1e9
        print(String(format: "PlayoutBudget.admit: %.0f ns/call (%d admitted)", ns, admitted))
        XCTAssertLessThan(ns, 1000, "per-audio-packet cost should be nanoseconds")
    }

    func testSquelchCalibratorAddCost() {
        // A whole calibration is 30 samples; time many of them.
        let iterations = 10_000
        let start = Date()
        for _ in 0..<iterations {
            var c = SquelchCalibrator()
            for i in 0..<30 { c.add(powerDb: Float(-90 + i % 5)) }
            _ = c.result
        }
        let us = Date().timeIntervalSince(start) / Double(iterations) * 1e6
        print(String(format: "SquelchCalibrator: %.1f us per complete 30-sample calibration", us))
        XCTAssertLessThan(us, 100)
    }
}
