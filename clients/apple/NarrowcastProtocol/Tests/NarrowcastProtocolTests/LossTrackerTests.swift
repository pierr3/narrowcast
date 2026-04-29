import XCTest
@testable import NarrowcastProtocol

final class LossTrackerTests: XCTestCase {

    func testFirstSeqMarkReturnsNil() {
        let t = LossTracker()
        XCTAssertNil(t.observeSeqMark(audioSent: 50, fftSent: 10, statusSent: 4))
    }

    func testZeroLossWindow() {
        let t = LossTracker()
        let t0 = Date()
        _ = t.observeSeqMark(audioSent: 50, fftSent: 10, statusSent: 4, now: t0)
        for _ in 0..<50 { t.recordReceived(.audio) }
        for _ in 0..<10 { t.recordReceived(.fft) }
        for _ in 0..<4 { t.recordReceived(.status) }

        let s = t.observeSeqMark(audioSent: 100, fftSent: 20, statusSent: 8, now: t0.addingTimeInterval(1.0))
        XCTAssertNotNil(s)
        XCTAssertEqual(s?.audioLossPct, 0)
        XCTAssertEqual(s?.fftLossPct, 0)
    }

    func testHalfAudioLost() {
        let t = LossTracker()
        let t0 = Date()
        _ = t.observeSeqMark(audioSent: 0, fftSent: 0, statusSent: 0, now: t0)
        for _ in 0..<25 { t.recordReceived(.audio) } // server sent 50, we got 25 = 50% loss

        let s = t.observeSeqMark(audioSent: 50, fftSent: 0, statusSent: 0, now: t0.addingTimeInterval(1.0))
        XCTAssertEqual(s?.audioLossPct, 50)
    }

    func testCounterResetTreatedAsZero() {
        let t = LossTracker()
        let t0 = Date()
        _ = t.observeSeqMark(audioSent: 100, fftSent: 0, statusSent: 0, now: t0)
        // simulate server pipeline restart: counter goes back to a smaller number
        let s = t.observeSeqMark(audioSent: 5, fftSent: 0, statusSent: 0, now: t0.addingTimeInterval(1.0))
        XCTAssertEqual(s?.audioLossPct, 0)
    }
}
