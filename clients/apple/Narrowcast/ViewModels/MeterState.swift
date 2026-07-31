import Foundation
import Combine
import QuartzCore

// MeterState holds the S-meter reading and its peak-hold companion.
//
// It exists as its own ObservableObject purely to scope SwiftUI invalidation:
// status frames arrive ~10×/s, and while these values lived on
// ConnectionViewModel every frame rebuilt the entire ListenView body — the
// favourites bar, the segmented picker, the formatted frequency labels, and an
// updateUIView pass on the Metal spectrum — to move one bar a pixel. Only the
// meter view observes this object, so only the meter redraws.
@MainActor
final class MeterState: ObservableObject {

    /// Current level in dB, as reported by the server's status datagram.
    @Published private(set) var db: Float = -120

    /// Peak-hold companion: snaps up on a rising edge, then decays at a fixed
    /// dB/sec so the UI shows a falling tick after a burst transmission —
    /// classic analog meter behaviour.
    @Published private(set) var peakDb: Float = -120

    /// dB per second decay after a peak. 12 dB/s holds the tick near the signal
    /// level for about a second of useful read time before falling away.
    private let peakDecayPerSec: Float = 12

    private var lastUpdate: CFTimeInterval = CACurrentMediaTime()

    func update(db value: Float) {
        let now = CACurrentMediaTime()
        let dt = Float(max(0, now - lastUpdate))
        lastUpdate = now

        if value != db { db = value }

        let decayed = value >= peakDb ? value : max(value, peakDb - peakDecayPerSec * dt)
        if decayed != peakDb { peakDb = decayed }
    }

    func reset() {
        db = -120
        peakDb = -120
        lastUpdate = CACurrentMediaTime()
    }
}
