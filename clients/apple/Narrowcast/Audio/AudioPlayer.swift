import Foundation
import AVFoundation
import QuartzCore
import NarrowcastClient
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
// depth *is* the playback latency — and it only moves one way on its own: a
// stall adds packets that never come back out. Left unmanaged it ratchets, and
// playback ends up sitting a quarter-second behind the radio for the rest of the
// session, which reads as "laggy" even on a LAN with zero packet loss. So the
// backlog is tracked explicitly and used for two things:
//
//   * Which packets are worth playing at all is decided by PlayoutBudget, which
//     sheds both bursts past the ceiling and standing latency below it.
//   * When the backlog reaches zero (squelch closed, transmission over) playback
//     pauses and the preroll re-arms, so the next transmission starts with a
//     cushion instead of playing each packet the instant it lands.
public final class AudioPlayer: @unchecked Sendable {

    public let sampleRate: Double

    private let engine = AVAudioEngine()
    private let player = AVAudioPlayerNode()
    private let format: AVAudioFormat

    private let prerollPackets: Int

    private let stateLock: OSAllocatedUnfairLock<State>
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
        /// Bumped on every start/stop transition, and checked by the deferred
        /// pause below. A pause decided when the queue ran dry must not land
        /// after a new transmission has already restarted playback: that leaves
        /// the node paused while `playing` is true, so nothing ever calls play()
        /// again and the backlog never drains — silent for the rest of the
        /// session, with every subsequent packet shed as if it were late.
        var generation: UInt64 = 0
        /// Decides which packets are worth playing. Pure logic, and tested
        /// separately — see PlayoutBudget.
        var budget: PlayoutBudget
    }

    /// - Parameters:
    ///   - maxLatency: backlog ceiling. 400 ms is past where a monitoring
    ///     listener notices lag, and generous enough to keep a genuinely lossy
    ///     link intact, since standing latency is handled separately.
    ///   - targetLatency: what shedding drains to, and the most standing latency
    ///     tolerated. 120 ms is a comfortable cushion for wifi jitter.
    public init(sampleRate: Int,
                prerollPackets: Int = 3,
                maxLatency: TimeInterval = 0.4,
                targetLatency: TimeInterval = 0.12) throws {
        self.sampleRate = Double(sampleRate)
        self.prerollPackets = prerollPackets
        self.stateLock = OSAllocatedUnfairLock(
            initialState: State(budget: PlayoutBudget(
                maxFrames: Int(Double(sampleRate) * maxLatency),
                targetFrames: Int(Double(sampleRate) * targetLatency)
            ))
        )

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
        stateLock.withLock { (Double($0.queuedFrames) / sampleRate, $0.budget.droppedPackets) }
    }

    public func stop() {
        player.stop()
        engine.stop()
        stateLock.withLock { state in
            state.prerolledPackets = 0
            state.playing = false
            state.queuedFrames = 0
            // Invalidate any pause still queued, so it can't fire against a
            // player that has since been restarted.
            state.generation &+= 1
            state.budget.reset()
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

            guard state.budget.admit(queuedFrames: state.queuedFrames,
                                     now: CACurrentMediaTime()) else {
                return .drop
            }

            state.queuedFrames += samples.count

            if !state.playing {
                state.prerolledPackets += 1
                if state.prerolledPackets >= prerollPackets {
                    state.playing = true
                    state.generation &+= 1
                    return .scheduleAndStart
                }
            }
            return .schedule
        }

        guard action != .drop else { return }

        // Schedule before starting: the other order can spin the engine a tick on
        // an empty player and mute briefly. This call is synchronous, so the
        // buffer is queued by the time anything below runs.
        let frames = samples.count
        player.scheduleBuffer(buffer, completionCallbackType: .dataPlayedBack) { [weak self] _ in
            self?.framesPlayed(frames)
        }

        // Transport calls go through controlQueue so play and pause can never run
        // concurrently from the enqueue path and a completion callback.
        if action == .scheduleAndStart {
            controlQueue.async { [weak self] in
                guard let self else { return }
                do {
                    if !self.engine.isRunning { try self.engine.start() }
                    self.player.play()
                } catch {
                    NSLog("[narrowcast] audio engine start: \(error)")
                }
            }
        }
    }

    /// Called from the engine's completion callback once a buffer has actually
    /// been played out.
    private func framesPlayed(_ frames: Int) {
        let pauseGeneration = stateLock.withLock { state -> UInt64? in
            state.queuedFrames = max(0, state.queuedFrames - frames)
            guard state.playing, state.queuedFrames == 0 else { return nil }
            // Nothing left to play. Re-arm the preroll so the next transmission
            // rebuilds a cushion rather than running with zero slack, where any
            // jitter is an audible chop.
            state.playing = false
            state.prerolledPackets = 0
            state.generation &+= 1
            return state.generation
        }
        guard let pauseGeneration else { return }
        controlQueue.async { [weak self] in
            guard let self else { return }
            // Skip the pause if a transmission has restarted playback since this
            // was decided — see State.generation.
            let stillIdle = self.stateLock.withLock {
                $0.generation == pauseGeneration && !$0.playing
            }
            if stillIdle { self.player.pause() }
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
