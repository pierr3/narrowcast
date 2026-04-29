import XCTest
@testable import NarrowcastProtocol

final class ClientMessageTests: XCTestCase {

    func testHelloEncodesTypeAndVersion() {
        let bytes = ClientMessage.hello(version: 1).encode()
        XCTAssertEqual(Array(bytes), [0x30, 0x01])
    }

    func testStartStopSingleByte() {
        XCTAssertEqual(Array(ClientMessage.start.encode()), [0x20])
        XCTAssertEqual(Array(ClientMessage.stop.encode()), [0x21])
    }

    func testSetFrequencyLittleEndian() {
        // 144_800_000 = 0x089A1779 → LE bytes 79 17 9A 08 00 00 00 00
        let bytes = ClientMessage.setFrequency(hz: 144_800_000).encode()
        var expected: [UInt8] = [0x10]
        var hz: UInt64 = (144_800_000 as UInt64).littleEndian
        withUnsafeBytes(of: &hz) { expected.append(contentsOf: $0) }
        XCTAssertEqual(Array(bytes), expected)
    }

    func testSetModeAM() {
        XCTAssertEqual(Array(ClientMessage.setMode(.am).encode()), [0x11, 0x02])
    }

    func testSetSquelchFloat32LE() {
        // -80.0 in float32 LE = 0x00 0x00 0xA0 0xC2
        let bytes = ClientMessage.setSquelch(dBm: -80).encode()
        XCTAssertEqual(Array(bytes), [0x12, 0x00, 0x00, 0xA0, 0xC2])
    }

    func testQualityReportLayout() {
        let bytes = ClientMessage.qualityReport(audioLossPct: 5, fftLossPct: 17, windowMs: 2000).encode()
        // 0x14 type, 0x05, 0x11, then 2000 = 0x07D0 LE → 0xD0 0x07
        XCTAssertEqual(Array(bytes), [0x14, 0x05, 0x11, 0xD0, 0x07])
    }

    func testAuthHashLength() {
        let pw = PasswordHash.sha256("hunter2")
        XCTAssertEqual(pw.count, 32)
        let bytes = ClientMessage.auth(passwordHash: pw).encode()
        XCTAssertEqual(bytes.count, 33)
        XCTAssertEqual(bytes[0], 0x32)
    }
}
