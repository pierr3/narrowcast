import XCTest
@testable import NarrowcastClient

final class SquelchCalibratorTests: XCTestCase {

    private func calibrate(_ samples: [Float],
                           margin: Float = SquelchCalibrator.defaultMarginDb) -> SquelchCalibrator.Result? {
        var c = SquelchCalibrator(marginDb: margin)
        for s in samples { c.add(powerDb: s) }
        return c.result
    }

    // A dead channel: everything is noise, so the floor is the noise.
    func testQuietChannelFindsTheFloor() {
        let noise: [Float] = (0..<30).map { _ in Float.random(in: -95...(-92)) }
        guard let r = calibrate(noise) else { return XCTFail("no result") }
        XCTAssertEqual(r.noiseFloorDb, -94, accuracy: 2)
        XCTAssertEqual(r.thresholdDb, r.noiseFloorDb + 6, accuracy: 0.01)
    }

    // The case that rules out a mean or a median: sampling straight through a
    // transmission. The threshold must still land just above the noise, not up
    // among the signal.
    func testBusyChannelStillFindsTheFloor() {
        var samples: [Float] = (0..<12).map { _ in Float.random(in: -95...(-92)) }
        samples += (0..<18).map { _ in Float.random(in: -60...(-55)) }  // 60% busy

        guard let r = calibrate(samples) else { return XCTFail("no result") }
        XCTAssertLessThan(r.thresholdDb, -80,
                          "threshold landed in the signal, so weak traffic would be gated out")
        XCTAssertGreaterThan(r.thresholdDb, r.noiseFloorDb)
    }

    // Even a channel busy most of the time should calibrate — the percentile is
    // chosen precisely so this works. The guarantee is busy < (1 - percentile),
    // so 10% noise is right at the edge of what must still succeed.
    func testVeryBusyChannelUpToEightyPercent() {
        var samples: [Float] = (0..<6).map { _ in Float.random(in: -96...(-94)) }
        samples += (0..<24).map { _ in Float.random(in: -50...(-45)) }  // 80% busy

        guard let r = calibrate(samples) else { return XCTFail("no result") }
        XCTAssertEqual(r.noiseFloorDb, -95, accuracy: 2)
    }

    // A threshold from three samples is a coin toss dressed up as a measurement.
    func testRefusesToGuessFromTooFewSamples() {
        XCTAssertNil(calibrate([-90, -91, -89]))
        var c = SquelchCalibrator()
        XCTAssertFalse(c.hasEnoughSamples)
        for _ in 0..<SquelchCalibrator.minimumSamples { c.add(powerDb: -90) }
        XCTAssertTrue(c.hasEnoughSamples)
        XCTAssertNotNil(c.result)
    }

    // The gate closes 3 dB below the threshold, so the margin has to clear that.
    func testMarginClearsServerHysteresis() {
        guard let r = calibrate(Array(repeating: -90, count: 30)) else {
            return XCTFail("no result")
        }
        XCTAssertGreaterThan(r.thresholdDb - r.noiseFloorDb, 3,
                             "threshold sits inside the hysteresis band")
    }

    func testCustomMargin() {
        guard let r = calibrate(Array(repeating: Float(-100), count: 20), margin: 10) else {
            return XCTFail("no result")
        }
        XCTAssertEqual(r.thresholdDb, -90, accuracy: 0.01)
    }

    // Status frames carry a float straight off the wire; a NaN must not become
    // the threshold.
    func testIgnoresNonFiniteSamples() {
        var c = SquelchCalibrator()
        for _ in 0..<20 { c.add(powerDb: -90) }
        c.add(powerDb: .nan)
        c.add(powerDb: .infinity)
        guard let r = c.result else { return XCTFail("no result") }
        XCTAssertEqual(r.sampleCount, 20)
        XCTAssertEqual(r.noiseFloorDb, -90, accuracy: 0.01)
    }

    func testResetDiscardsSamples() {
        var c = SquelchCalibrator()
        for _ in 0..<30 { c.add(powerDb: -90) }
        c.reset()
        XCTAssertEqual(c.sampleCount, 0)
        XCTAssertNil(c.result)
    }

    // A single steady level is the degenerate case — floor equals that level.
    func testConstantInput() {
        guard let r = calibrate(Array(repeating: Float(-77.5), count: 30)) else {
            return XCTFail("no result")
        }
        XCTAssertEqual(r.noiseFloorDb, -77.5, accuracy: 0.01)
    }
}
