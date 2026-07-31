import Foundation
import AVFoundation
import os

#if canImport(UIKit)
import UIKit
#endif

// AudioPlayer schedules decoded PCM buffers on an AVAudioPlayerNode. The
// engine handles per-sample scheduling internally — much more robust than
// a hand-rolled ring + source node, which has to cope with the render
// callback's variable frameCount and is prone to underruns when buffer
// timing doesn't line up perfectly.
//
// Flow per decoded Opus packet:
//   decoder -> AudioPlayer.enqueue([Float]) -> wrap in AVAudioPCMBuffer
//   -> player.scheduleBuffer(buffer)  // engine plays in order
//
// The engine drains that queue at the device clock and nothing else, so queue
// depth *is* the playback latency — and it only moves one way: a stall adds
// packets that never come back out. Left unmanaged it ratchets, and after a few
// hiccups playback sits half a second behind the radio for the rest of the
// session, which reads as "laggy" even on a LAN with zero packet loss. So the
// backlog is tracked explicitly and used for two things:
//
//   * Above `maxLatency`, incoming packets are dropped until the backlog is back
//     down to `targetLatency`. Shedding realtime audio to stay current is the
//     whole design; buffering it is not.
//   * When the backlog reaches zero (squelch closed, transmission over) playback
//     pauses and the preroll re-arms, so the next transmission starts with a
//     cushion instead of playing each packet the instant it lands.
public final class AudioPlayer: @unchecked Sendable {

    public let sampleRate: Double

    private let engine = AVAudioEngine()
    private let player = AVAudioPlayerNode()
    private let format: AVAudioFormat

    private let prerollPackets: Int
    /// Backlog ceiling before shedding starts, and the level it sheds down to.
    /// 400 ms is past where a monitoring listener notices lag; 120 ms is a
    /// comfortable cushion for wifi jitter.
    private let maxFrames: Int
    private let targetFrames: Int

    private let stateLock = OSAllocatedUnfairLock<State>(initialState: State())
    /// Transport control issued from the engine's completion callbacks. Kept off
    /// that callback's own queue, which is not somewhere to call back into the
    /// engine from.
    private let controlQueue = DispatchQueue(label: "narrowcast.audio.control")

    private struct State {
        var prerolledPackets: Int = 0
        var playing: Bool = false
        var sessionConfigured: Bool = false
        /// Frames scheduled but not yet played back — the current latency.
        var queuedFrames: Int = 0
        /// True while shedding down to targetFrames after exceeding maxFrames.
        var shedding: Bool = false
        var droppedPackets: Int = 0
    }

    public init(sampleRate: Int,
                prerollPackets: Int = 3,
                maxLatency: TimeInterval = 0.4,
                targetLatency: TimeInterval = 0.12) throws {
        self.sampleRate = Double(sampleRate)
        self.prerollPackets = prerollPackets
        self.maxFrames = Int(Double(sampleRate) * maxLatency)
        self.targetFrames = Int(Double(sampleRate) * targetLatency)

        let fmt = AVAudioFormat(
            commonFormat: .pcmFormatFloat32,
            sampleRate: Double(sampleRate),
            channels: 1,
            interleaved: false
        )!
        self.format = fmt

        engine.attach(player)
        engine.connect(player, to: engine.mainMixerNode, format: fmt)
    }

    /// Current playback backlog in seconds plus how many packets have been shed
    /// to keep it bounded. Diagnostics only.
    public var latency: (seconds: Double, droppedPackets: Int) {
        stateLock.withLock { (Double($0.queuedFrames) / sampleRate, $0.droppedPackets) }
    }

    public func stop() {
        player.stop()
        engine.stop()
        stateLock.withLock { state in
            state.prerolledPackets = 0
            state.playing = false
            state.queuedFrames = 0
            state.shedding = false
        }
    }

    /// Schedule a chunk of decoded PCM. The first few enqueues build a cushion
    /// without playing; the engine starts at the preroll threshold.
    public func enqueue(_ samples: [Float]) {
        guard !samples.isEmpty else { return }
        // Built before the backlog is booked, so a failed allocation can't leave
        // frames counted as queued that will never be played back.
        guard let buffer = makeBuffer(from: samples) else { return }

        enum Action { case drop, schedule, scheduleAndStart }

        let action = stateLock.withLock { state -> Action in
            if !state.sessionConfigured {
                #if canImport(UIKit)
                // .allowBluetoothA2DP enables routing to Bluetooth speakers
                // / AirPods (high-quality output profile). .allowAirPlay
                // mirrors via AirPlay receivers. .playback alone defaults
                // to handset speaker only on some routes.
                try? AVAudioSession.sharedInstance().setCategory(
                    .playback,
                    mode: .default,
                    options: [.allowBluetoothA2DP, .allowAirPlay]
                )
                try? AVAudioSession.sharedInstance().setActive(true)
                #endif
                state.sessionConfigured = true
            }

            // Latency guard, with hysteresis between max and target so a burst
            // is shed once instead of oscillating around the ceiling.
            if state.shedding {
                if state.queuedFrames > targetFrames {
                    state.droppedPackets += 1
                    return .drop
                }
                state.shedding = false
            } else if state.queuedFrames > maxFrames {
                state.shedding = true
                state.droppedPackets += 1
                return .drop
            }

            state.queuedFrames += samples.count

            if !state.playing {
                state.prerolledPackets += 1
                if state.prerolledPackets >= prerollPackets {
                    state.playing = true
                    return .scheduleAndStart
                }
            }
            return .schedule
        }

        guard action != .drop else { return }

        // Schedule first, then start the engine. If we started before
        // scheduling, the engine could spin a tick on an empty player and
        // mute briefly.
        let frames = samples.count
        player.scheduleBuffer(buffer, completionCallbackType: .dataPlayedBack) { [weak self] _ in
            self?.framesPlayed(frames)
        }

        if action == .scheduleAndStart {
            do {
                if !engine.isRunning { try engine.start() }
                player.play()
            } catch {
                NSLog("[narrowcast] audio engine start: \(error)")
            }
        }
    }

    /// Called from the engine's completion callback once a buffer has actually
    /// been played out.
    private func framesPlayed(_ frames: Int) {
        let ranDry = stateLock.withLock { state -> Bool in
            state.queuedFrames = max(0, state.queuedFrames - frames)
            guard state.playing, state.queuedFrames == 0 else { return false }
            // Nothing left to play. Re-arm the preroll so the next transmission
            // rebuilds a cushion rather than running with zero slack, where any
            // jitter is an audible chop.
            state.playing = false
            state.prerolledPackets = 0
            return true
        }
        if ranDry {
            controlQueue.async { [weak self] in self?.player.pause() }
        }
    }

    private func makeBuffer(from samples: [Float]) -> AVAudioPCMBuffer? {
        guard let buf = AVAudioPCMBuffer(pcmFormat: format,
                                         frameCapacity: AVAudioFrameCount(samples.count)) else {
            return nil
        }
        buf.frameLength = AVAudioFrameCount(samples.count)
        guard let dst = buf.floatChannelData?[0] else { return nil }
        samples.withUnsafeBufferPointer { src in
            if let base = src.baseAddress {
                dst.update(from: base, count: samples.count)
            }
        }
        return buf
    }
}
