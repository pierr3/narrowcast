import Foundation
import MediaPlayer
import AVFoundation
import NarrowcastProtocol

// NowPlayingController owns the integration with iOS media surfaces:
// the lock-screen / Control Center "Now Playing" tile and the system
// audio-interruption pipeline (phone calls, Siri, other apps stealing
// the route). Everything that's not strictly local-audio-output lives
// here so AudioPlayer can stay a thin scheduling primitive.
//
// Lifecycle:
//   - activate() once on connect; updateMetadata(...) on state changes;
//     deactivate() on disconnect.
//   - Remote play/pause + interruption notifications call back into the
//     supplied closures so this stays decoupled from ConnectionViewModel.
@MainActor
final class NowPlayingController {

    var onPlay: (() -> Void)?
    var onPause: (() -> Void)?

    private var registered = false
    private var interruptionObserver: NSObjectProtocol?

    func activate() {
        guard !registered else { return }
        registered = true

        let cc = MPRemoteCommandCenter.shared()
        // Disable everything we don't handle so the lock-screen UI
        // doesn't render dead skip/seek buttons.
        cc.skipForwardCommand.isEnabled = false
        cc.skipBackwardCommand.isEnabled = false
        cc.nextTrackCommand.isEnabled = false
        cc.previousTrackCommand.isEnabled = false
        cc.changePlaybackPositionCommand.isEnabled = false

        cc.playCommand.isEnabled = true
        cc.playCommand.addTarget { [weak self] _ in
            guard let self else { return .commandFailed }
            self.onPlay?()
            return .success
        }
        cc.pauseCommand.isEnabled = true
        cc.pauseCommand.addTarget { [weak self] _ in
            guard let self else { return .commandFailed }
            self.onPause?()
            return .success
        }
        cc.togglePlayPauseCommand.isEnabled = true
        cc.togglePlayPauseCommand.addTarget { [weak self] _ in
            guard let self else { return .commandFailed }
            // Lock-screen toggle: query current state from the info center.
            let info = MPNowPlayingInfoCenter.default().nowPlayingInfo
            let rate = info?[MPNowPlayingInfoPropertyPlaybackRate] as? Double ?? 0
            if rate > 0 { self.onPause?() } else { self.onPlay?() }
            return .success
        }

        interruptionObserver = NotificationCenter.default.addObserver(
            forName: AVAudioSession.interruptionNotification,
            object: AVAudioSession.sharedInstance(),
            queue: .main
        ) { [weak self] note in
            guard let self else { return }
            self.handleInterruption(note)
        }
    }

    func deactivate() {
        guard registered else { return }
        registered = false

        let cc = MPRemoteCommandCenter.shared()
        cc.playCommand.removeTarget(nil)
        cc.pauseCommand.removeTarget(nil)
        cc.togglePlayPauseCommand.removeTarget(nil)

        if let interruptionObserver {
            NotificationCenter.default.removeObserver(interruptionObserver)
        }
        interruptionObserver = nil

        MPNowPlayingInfoCenter.default().nowPlayingInfo = nil
    }

    /// Update lock-screen / Control Center metadata. `playing` drives the
    /// playback-rate field that the UI uses to render the play vs pause
    /// glyph — keep it in sync with the actual streaming state.
    func updateMetadata(stationName: String, freqHz: UInt64, mode: DemodMode, playing: Bool) {
        let mhz = Double(freqHz) / 1_000_000
        let title = String(format: "%.3f MHz · %@", mhz, mode.displayName)
        let info: [String: Any] = [
            MPMediaItemPropertyTitle: title,
            MPMediaItemPropertyArtist: stationName,
            MPMediaItemPropertyAlbumTitle: "Narrowcast",
            MPNowPlayingInfoPropertyIsLiveStream: true,
            MPNowPlayingInfoPropertyPlaybackRate: playing ? 1.0 : 0.0,
        ]
        MPNowPlayingInfoCenter.default().nowPlayingInfo = info
    }

    /// Update only the playback-rate so the lock-screen glyph flips.
    func setPlaying(_ playing: Bool) {
        guard var info = MPNowPlayingInfoCenter.default().nowPlayingInfo else { return }
        info[MPNowPlayingInfoPropertyPlaybackRate] = playing ? 1.0 : 0.0
        MPNowPlayingInfoCenter.default().nowPlayingInfo = info
    }

    private func handleInterruption(_ note: Notification) {
        guard
            let info = note.userInfo,
            let raw = info[AVAudioSessionInterruptionTypeKey] as? UInt,
            let type = AVAudioSession.InterruptionType(rawValue: raw)
        else { return }

        switch type {
        case .began:
            // Phone call / Siri / other app took the route. Pause local
            // output so audio engine isn't fighting the system.
            onPause?()
        case .ended:
            // Resume only if iOS says so (`.shouldResume`). Without that
            // flag we'd un-pause on top of e.g. a dialed call ringing.
            guard
                let optsRaw = info[AVAudioSessionInterruptionOptionKey] as? UInt
            else { return }
            let opts = AVAudioSession.InterruptionOptions(rawValue: optsRaw)
            if opts.contains(.shouldResume) {
                onPlay?()
            }
        @unknown default:
            break
        }
    }
}

private extension DemodMode {
    var displayName: String {
        switch self {
        case .nfm: return "NFM"
        case .wfm: return "WFM"
        case .am: return "AM"
        }
    }
}
