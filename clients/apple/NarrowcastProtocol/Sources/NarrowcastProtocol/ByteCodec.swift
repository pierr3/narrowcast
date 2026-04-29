import Foundation

// Wire format mirrors pkg/protocol on the Go side. Multi-byte numerics are
// little-endian, except FFT's bin count which the server emits big-endian
// (see cmd/narrowcast/main.go: `dgram[1] = byte(len(bins) >> 8)`).

enum WireError: Error {
    case truncated
    case unknownType(UInt8)
    case badEnum(UInt8)
}

struct ByteReader {
    let data: Data
    var offset: Int = 0

    var remaining: Int { data.count - offset }

    mutating func u8() throws -> UInt8 {
        guard remaining >= 1 else { throw WireError.truncated }
        defer { offset += 1 }
        return data[data.startIndex + offset]
    }

    mutating func u16LE() throws -> UInt16 {
        guard remaining >= 2 else { throw WireError.truncated }
        let b0 = UInt16(data[data.startIndex + offset])
        let b1 = UInt16(data[data.startIndex + offset + 1])
        offset += 2
        return b0 | (b1 << 8)
    }

    mutating func u16BE() throws -> UInt16 {
        guard remaining >= 2 else { throw WireError.truncated }
        let b0 = UInt16(data[data.startIndex + offset])
        let b1 = UInt16(data[data.startIndex + offset + 1])
        offset += 2
        return (b0 << 8) | b1
    }

    mutating func u32LE() throws -> UInt32 {
        guard remaining >= 4 else { throw WireError.truncated }
        var v: UInt32 = 0
        for i in 0..<4 {
            v |= UInt32(data[data.startIndex + offset + i]) << (8 * i)
        }
        offset += 4
        return v
    }

    mutating func u64LE() throws -> UInt64 {
        guard remaining >= 8 else { throw WireError.truncated }
        var v: UInt64 = 0
        for i in 0..<8 {
            v |= UInt64(data[data.startIndex + offset + i]) << (8 * i)
        }
        offset += 8
        return v
    }

    mutating func f32LE() throws -> Float {
        let bits = try u32LE()
        return Float(bitPattern: bits)
    }

    mutating func bytes(_ n: Int) throws -> Data {
        guard remaining >= n else { throw WireError.truncated }
        defer { offset += n }
        return data.subdata(in: (data.startIndex + offset)..<(data.startIndex + offset + n))
    }

    mutating func remainingBytes() -> Data {
        defer { offset = data.count }
        return data.subdata(in: (data.startIndex + offset)..<data.endIndex)
    }
}

struct ByteWriter {
    private(set) var data: Data

    init(capacity: Int = 64) {
        self.data = Data()
        self.data.reserveCapacity(capacity)
    }

    mutating func u8(_ v: UInt8) {
        data.append(v)
    }

    mutating func u16LE(_ v: UInt16) {
        data.append(UInt8(v & 0xff))
        data.append(UInt8((v >> 8) & 0xff))
    }

    mutating func u32LE(_ v: UInt32) {
        for i in 0..<4 {
            data.append(UInt8((v >> (8 * i)) & 0xff))
        }
    }

    mutating func u64LE(_ v: UInt64) {
        for i in 0..<8 {
            data.append(UInt8((v >> (8 * i)) & 0xff))
        }
    }

    mutating func f32LE(_ v: Float) {
        u32LE(v.bitPattern)
    }

    mutating func bytes(_ b: Data) {
        data.append(b)
    }
}
