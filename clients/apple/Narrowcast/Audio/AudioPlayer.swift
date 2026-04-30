import Foundation
import AVFoundation
import os

#if canImport(UIKit)
import UIKit
#endif

// AudioPlayer is the playback side of the audio pipeline. AVAudioEngine +
// AVAudioSourceNode + a small lock-protected ring buffer. Render callback
// runs on the realtime audio thread and reads from the ring; the audio queue
// writes to it from off-main. os_unfair_lock (via OSAllocatedUnfairLock) is
// used for the few-microsecond critical section because it handles priority
// inversion correctly — NSLock can starve the realtime thread.
//
// Preroll: the engine doesn't start until ~60 ms of decoded PCM (3 packets)
// is sitting in the ring. Without this, the render callback drains the
// empty ring on first tick, gaps out, the next packet arrives, plays, then
// drains the ring again — choppy.
public final class AudioPlayer: @unchecked Sendable {

    public let sampleRate: Double

    private let engine = AVAudioEngine()
    private let sourceNode: AVAudioSourceNode
    private let ring: PCMRing

    private let prerollSamples: Int
    private let startedFlag = OSAllocatedUnfairLock(initialState: false)
    private let pendingStart: () -> Void

    public init(sampleRate: Int, prerollMs: Int = 60) throws {
        self.sampleRate = Double(sampleRate)
        self.prerollSamples = sampleRate * prerollMs / 1000
        self.ring = PCMRing(capacity: sampleRate)  // 1 s headroom

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
                memset(dst.advanced(by: read), 0, (n - read) * MemoryLayout<Float>.size)
            }
            return noErr
        }

        engine.attach(sourceNode)
        engine.connect(sourceNode, to: engine.mainMixerNode, format: format)

        // Capture self-less closure so it can be called from the audio queue
        // when the ring crosses prerollSamples.
        let engineRef = engine
        let flagRef = startedFlag
        self.pendingStart = {
            #if canImport(UIKit)
            try? AVAudioSession.sharedInstance().setCategory(.playback, mode: .default, options: [])
            try? AVAudioSession.sharedInstance().setActive(true)
            #endif
            let alreadyStarted = flagRef.withLock { state -> Bool in
                if state { return true }
                state = true
                return false
            }
            if !alreadyStarted {
                try? engineRef.start()
            }
        }
    }

    /// Stop the engine and clear pending audio. Safe to call from any thread.
    public func stop() {
        engine.stop()
        ring.clear()
        startedFlag.withLock { $0 = false }
    }

    /// Push decoded PCM into the ring. Triggers engine start once the ring
    /// crosses the preroll threshold.
    public func enqueue(_ samples: [Float]) {
        ring.write(samples)
        let started = startedFlag.withLock { $0 }
        if !started && ring.count >= prerollSamples {
            pendingStart()
        }
    }
}

// MARK: - Ring buffer

final class PCMRing: @unchecked Sendable {
    private let buffer: UnsafeMutableBufferPointer<Float>
    private let capacity: Int
    private var head: Int = 0
    private var tail: Int = 0
    private let lock = OSAllocatedUnfairLock()

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
        lock.withLock { (head - tail + capacity) % capacity }
    }

    func write(_ samples: [Float]) {
        lock.withLock {
            for s in samples {
                buffer[head] = s
                head = (head + 1) % capacity
                if head == tail {
                    tail = (tail + 1) % capacity  // overwrite oldest
                }
            }
        }
    }

    func read(into dst: UnsafeMutablePointer<Float>, count want: Int) -> Int {
        lock.withLock {
            var produced = 0
            while produced < want && tail != head {
                dst[produced] = buffer[tail]
                tail = (tail + 1) % capacity
                produced += 1
            }
            return produced
        }
    }

    func clear() {
        lock.withLock {
            head = 0
            tail = 0
        }
    }
}
