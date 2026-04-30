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
// Preroll: schedule the first ~3 packets without calling player.play(); the
// engine queues them. play() unmutes on the third enqueue. After that the
// engine pulls from its own queue at the device clock rate.
public final class AudioPlayer: @unchecked Sendable {

    public let sampleRate: Double

    private let engine = AVAudioEngine()
    private let player = AVAudioPlayerNode()
    private let format: AVAudioFormat

    private let prerollFrames: Int  // packets, not samples
    private let stateLock = OSAllocatedUnfairLock<State>(initialState: State())

    private struct State {
        var prerolledPackets: Int = 0
        var playing: Bool = false
        var sessionConfigured: Bool = false
    }

    public init(sampleRate: Int, prerollPackets: Int = 3) throws {
        self.sampleRate = Double(sampleRate)
        self.prerollFrames = prerollPackets

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

    public func stop() {
        player.stop()
        engine.stop()
        stateLock.withLock { state in
            state.prerolledPackets = 0
            state.playing = false
        }
    }

    /// Schedule a chunk of decoded PCM. First few enqueues queue up without
    /// playing; engine starts at the preroll threshold.
    public func enqueue(_ samples: [Float]) {
        guard !samples.isEmpty else { return }
        guard let buffer = makeBuffer(from: samples) else { return }

        let shouldStart = stateLock.withLock { state -> Bool in
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
            if !state.playing {
                state.prerolledPackets &+= 1
                if state.prerolledPackets >= prerollFrames {
                    state.playing = true
                    return true
                }
            }
            return false
        }

        // Schedule first, then start the engine. If we started before
        // scheduling, the engine could spin a tick on an empty player and
        // mute briefly.
        player.scheduleBuffer(buffer, completionHandler: nil)

        if shouldStart {
            do {
                if !engine.isRunning { try engine.start() }
                player.play()
            } catch {
                NSLog("[narrowcast] audio engine start: \(error)")
            }
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
