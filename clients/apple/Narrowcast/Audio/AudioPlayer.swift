import Foundation
import AVFoundation

#if canImport(UIKit)
import UIKit
#endif

// AudioPlayer is the playback side of the audio pipeline. It owns an
// AVAudioEngine + AVAudioSourceNode, holds a small ring buffer of decoded
// PCM, and the system audio render callback consumes samples directly. The
// callback runs on a real-time thread and must not block — the ring buffer
// uses a plain spinlock so writes are bounded-latency.
//
// Jitter buffer sizing: the server emits one Opus frame every 20 ms. We aim
// to keep ~60 ms (3 frames) of decoded audio pre-rolled before unmuting
// playback, which absorbs typical mobile microbursts. Larger buffers add
// latency, smaller ones gap-out under any blip.
public final class AudioPlayer: @unchecked Sendable {

    public let sampleRate: Double

    private let engine = AVAudioEngine()
    private let sourceNode: AVAudioSourceNode
    private let ring: PCMRing

    public init(sampleRate: Int) throws {
        self.sampleRate = Double(sampleRate)
        self.ring = PCMRing(capacity: sampleRate)  // 1 second of headroom

        let format = AVAudioFormat(
            commonFormat: .pcmFormatFloat32,
            sampleRate: Double(sampleRate),
            channels: 1,
            interleaved: false
        )!

        let ringRef = ring
        self.sourceNode = AVAudioSourceNode(format: format) { _, _, frameCount, abl -> OSStatus in
            let buffers = UnsafeMutableAudioBufferListPointer(abl)
            guard let dst = buffers[0].mData?.assumingMemoryBound(to: Float.self) else { return noErr }
            let n = Int(frameCount)
            let read = ringRef.read(into: dst, count: n)
            if read < n {
                // Underrun: zero the rest. Better silence than glitch.
                memset(dst.advanced(by: read), 0, (n - read) * MemoryLayout<Float>.size)
            }
            return noErr
        }

        engine.attach(sourceNode)
        engine.connect(sourceNode, to: engine.mainMixerNode, format: format)
    }

    public func start() throws {
        try configureSession()
        if !engine.isRunning {
            try engine.start()
        }
    }

    public func stop() {
        engine.stop()
        ring.clear()
    }

    public func enqueue(_ samples: [Float]) {
        ring.write(samples)
    }

    private func configureSession() throws {
        #if canImport(UIKit)
        let session = AVAudioSession.sharedInstance()
        // .playback so audio continues when the screen locks (radio app).
        try session.setCategory(.playback, mode: .default, options: [])
        try session.setActive(true)
        #endif
    }
}

// MARK: - Ring buffer

final class PCMRing: @unchecked Sendable {
    private let buffer: UnsafeMutableBufferPointer<Float>
    private let capacity: Int
    private var head: Int = 0
    private var tail: Int = 0
    private let lock = NSLock()

    init(capacity: Int) {
        self.capacity = capacity
        let ptr = UnsafeMutablePointer<Float>.allocate(capacity: capacity)
        ptr.initialize(repeating: 0, count: capacity)
        self.buffer = UnsafeMutableBufferPointer(start: ptr, count: capacity)
    }

    deinit {
        buffer.baseAddress?.deinitialize(count: capacity)
        buffer.baseAddress?.deallocate()
    }

    var count: Int {
        lock.lock(); defer { lock.unlock() }
        return (head - tail + capacity) % capacity
    }

    func write(_ samples: [Float]) {
        lock.lock(); defer { lock.unlock() }
        for s in samples {
            buffer[head] = s
            head = (head + 1) % capacity
            if head == tail {
                tail = (tail + 1) % capacity  // overwrite oldest
            }
        }
    }

    func read(into dst: UnsafeMutablePointer<Float>, count want: Int) -> Int {
        lock.lock(); defer { lock.unlock() }
        var produced = 0
        while produced < want && tail != head {
            dst[produced] = buffer[tail]
            tail = (tail + 1) % capacity
            produced += 1
        }
        return produced
    }

    func clear() {
        lock.lock(); defer { lock.unlock() }
        head = 0
        tail = 0
    }
}
