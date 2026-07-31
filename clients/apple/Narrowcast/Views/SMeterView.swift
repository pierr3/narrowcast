import SwiftUI

// SMeterView is the only observer of MeterState, which is the point: status
// frames arrive ~10×/s and this is the sole part of the screen that has to
// redraw for them. See MeterState for what that scoping avoids.
struct SMeterView: View {
    @ObservedObject var meter: MeterState
    /// Passed in rather than observed: squelch changes when the user moves the
    /// slider, not at frame rate.
    let squelchDb: Float

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text("S").font(.caption).foregroundStyle(.secondary)
                Spacer()
                Text("\(Int(meter.db)) dB").font(.caption).monospacedDigit()
            }
            // The server pushes ~10 samples/sec; SwiftUI interpolates between
            // them so the meter slides at display rate instead of stepping in
            // 100 ms blocks. The peak-hold tick is an overlay positioned via
            // GeometryReader so it tracks the bar width whatever the layout.
            GeometryReader { geo in
                ZStack(alignment: .leading) {
                    ProgressView(value: fraction(meter.db))
                        .progressViewStyle(.linear)
                        .tint(meter.db > squelchDb ? .green : .gray)
                        .animation(.easeOut(duration: 0.12), value: meter.db)

                    Rectangle()
                        .fill(Color.orange)
                        .frame(width: 2, height: 14)
                        .offset(x: geo.size.width * fraction(meter.peakDb) - 1, y: -1)
                        .animation(.linear(duration: 0.12), value: meter.peakDb)
                }
            }
            .frame(height: 14)
        }
    }

    /// Map -120...0 dB onto 0...1.
    private func fraction(_ db: Float) -> Double {
        Double((max(-120, min(0, db)) + 120) / 120)
    }
}
