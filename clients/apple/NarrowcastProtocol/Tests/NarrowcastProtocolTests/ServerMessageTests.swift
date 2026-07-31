import XCTest
@testable import NarrowcastProtocol

final class ServerMessageTests: XCTestCase {

    func testWelcome() {
        // Build bytes via round-trip rather than hand-coded hex to avoid arithmetic errors.
        var bytes: [UInt8] = [0x31, 0x01]
        var lo: UInt64 = (24_000_000 as UInt64).littleEndian
        var hi: UInt64 = (1_766_000_000 as UInt64).littleEndian
        var sr: Float = 2_400_000.0
        withUnsafeBytes(of: &lo) { bytes.append(contentsOf: $0) }
        withUnsafeBytes(of: &hi) { bytes.append(contentsOf: $0) }
        withUnsafeBytes(of: &sr) { bytes.append(contentsOf: $0) }

        guard case .welcome(let v, let lo, let hi, let rate) = ServerMessage.decode(Data(bytes))! else {
            XCTFail("expected welcome"); return
        }
        XCTAssertEqual(v, 1)
        XCTAssertEqual(lo, 24_000_000)
        XCTAssertEqual(hi, 1_766_000_000)
        XCTAssertEqual(rate, 2_400_000)
    }

    func testStatusWithoutClientCount() {
        // 0x03, smeter=-50.0, squelch=-80.0, mode=NFM(0), freq=144_800_000
        var bytes: [UInt8] = [0x03]
        var s = Float(-50.0).bitPattern
        var q = Float(-80.0).bitPattern
        withUnsafeBytes(of: &s) { bytes.append(contentsOf: $0) }
        withUnsafeBytes(of: &q) { bytes.append(contentsOf: $0) }
        bytes += [0x00] // mode NFM
        var f: UInt64 = 144_800_000
        withUnsafeBytes(of: &f) { bytes.append(contentsOf: $0) }

        guard case .status(let smeter, let sq, let m, let freq, let cc) = ServerMessage.decode(Data(bytes))! else {
            XCTFail("expected status"); return
        }
        XCTAssertEqual(smeter, -50)
        XCTAssertEqual(sq, -80)
        XCTAssertEqual(m, .nfm)
        XCTAssertEqual(freq, 144_800_000)
        XCTAssertNil(cc)
    }

    func testStatusWithRelayClientCount() {
        var bytes: [UInt8] = [0x03]
        var s = Float(-30.0).bitPattern
        var q = Float(-90.0).bitPattern
        withUnsafeBytes(of: &s) { bytes.append(contentsOf: $0) }
        withUnsafeBytes(of: &q) { bytes.append(contentsOf: $0) }
        bytes += [0x02] // mode AM
        var f: UInt64 = 121_500_000
        withUnsafeBytes(of: &f) { bytes.append(contentsOf: $0) }
        bytes.append(0x07) // 7 clients

        guard case .status(_, _, let m, _, let cc) = ServerMessage.decode(Data(bytes))! else {
            XCTFail("expected status"); return
        }
        XCTAssertEqual(m, .am)
        XCTAssertEqual(cc, 7)
    }

    func testFFTBigEndianBinCount() {
        // 0x02, numBins=4 BE → 0x00 0x04, then 4 bytes
        let bytes: [UInt8] = [0x02, 0x00, 0x04, 0x10, 0x20, 0x30, 0x40]
        guard case .fft(let bins) = ServerMessage.decode(Data(bytes))! else {
            XCTFail("expected fft"); return
        }
        XCTAssertEqual(bins, [0x10, 0x20, 0x30, 0x40])
    }

    func testSeqMark() {
        var bytes: [UInt8] = [0x04]
        bytes += [0x10, 0x00, 0x00, 0x00] // 16
        bytes += [0x20, 0x00, 0x00, 0x00] // 32
        bytes += [0x40, 0x00, 0x00, 0x00] // 64
        guard case .seqMark(let a, let f, let s) = ServerMessage.decode(Data(bytes))! else {
            XCTFail("expected seqMark"); return
        }
        XCTAssertEqual(a, 16)
        XCTAssertEqual(f, 32)
        XCTAssertEqual(s, 64)
    }

    func testPlainAudioHasNoSequence() {
        guard case .audio(let opus, let seq) = ServerMessage.decode(Data([0x01, 0xAA, 0xBB]))! else {
            XCTFail("expected audio"); return
        }
        XCTAssertEqual(Array(opus), [0xAA, 0xBB])
        XCTAssertNil(seq, "0x01 carries no sequence number, so gaps can't be detected")
    }

    func testSequencedAudioSplitsSeqFromPayload() {
        // 0x05, seq=0x0102 LE, then the Opus bytes.
        guard case .audio(let opus, let seq) = ServerMessage.decode(Data([0x05, 0x02, 0x01, 0xAA, 0xBB]))! else {
            XCTFail("expected audio"); return
        }
        XCTAssertEqual(seq, 0x0102)
        XCTAssertEqual(Array(opus), [0xAA, 0xBB], "seq must not leak into the Opus packet")
    }

    func testSequencedAudioRejectsTruncatedHeader() {
        // A one-byte sequence number is malformed; decoding it as a payload
        // would hand libopus a corrupt packet.
        XCTAssertNil(ServerMessage.decode(Data([0x05, 0x02])))
    }

    func testSequencedAudioToleratesEmptyPayload() {
        guard case .audio(let opus, let seq) = ServerMessage.decode(Data([0x05, 0x00, 0x00]))! else {
            XCTFail("expected audio"); return
        }
        XCTAssertEqual(seq, 0)
        XCTAssertTrue(opus.isEmpty)
    }

    func testAuthOKAuthFail() {
        if case .authOK = ServerMessage.decode(Data([0x33]))! {} else { XCTFail() }
        if case .authFail = ServerMessage.decode(Data([0x34]))! {} else { XCTFail() }
    }

    func testUnknownTypePreserved() {
        guard case .unknown(let t) = ServerMessage.decode(Data([0xEE, 0x01, 0x02]))! else {
            XCTFail("expected unknown"); return
        }
        XCTAssertEqual(t, 0xEE)
    }
}
