import SwiftUI

// WaterfallView renders a scrolling spectrogram from u8 magnitude frames.
// Each frame is one row; older rows scroll downward as new frames arrive.
// Drawing is done in a SwiftUI Canvas: cheap enough at 10 fps × 1024 bins
// on modern iPhones (about 10k drawing ops/sec). If profiling ever shows
// this as a bottleneck, swap to a Metal texture + a single quad blit.
struct WaterfallView: View {
    let frames: [[UInt8]]   // newest at index 0
    let onTap: ((CGFloat) -> Void)?  // 0..1 horizontal fraction of the tap

    private let rowHeight: CGFloat = 2

    var body: some View {
        GeometryReader { geo in
            Canvas { ctx, size in
                guard !frames.isEmpty else { return }
                let w = size.width
                let h = size.height
                let visibleRows = max(1, Int(h / rowHeight))
                let rowsToDraw = min(visibleRows, frames.count)

                for rowIdx in 0..<rowsToDraw {
                    let frame = frames[rowIdx]
                    guard !frame.isEmpty else { continue }
                    let y = CGFloat(rowIdx) * rowHeight
                    let cellWidth = w / CGFloat(frame.count)
                    for (binIdx, mag) in frame.enumerated() {
                        let color = colorMap(magnitude: mag)
                        let rect = CGRect(
                            x: CGFloat(binIdx) * cellWidth,
                            y: y,
                            width: cellWidth + 0.5, // overdraw to avoid hairlines
                            height: rowHeight + 0.5
                        )
                        ctx.fill(Path(rect), with: .color(color))
                    }
                }
            }
            .background(Color.black)
            .contentShape(Rectangle())
            .onTapGesture { location in
                let frac = max(0, min(1, location.x / max(1, geo.size.width)))
                onTap?(frac)
            }
        }
    }

    /// Map u8 magnitude to a perceptually-linear color ramp. Uses an inferno-
    /// like curve: black → purple → red → orange → yellow → white. Built from
    /// piecewise linear segments rather than a 256-entry LUT to keep the file
    /// small and avoid an asset catalog.
    private func colorMap(magnitude m: UInt8) -> Color {
        let t = Double(m) / 255.0
        let r: Double, g: Double, b: Double
        switch t {
        case ..<0.25:
            // Black → deep purple
            let s = t / 0.25
            r = s * 0.4
            g = 0
            b = s * 0.5
        case ..<0.5:
            // Purple → red
            let s = (t - 0.25) / 0.25
            r = 0.4 + s * 0.6
            g = 0
            b = 0.5 - s * 0.5
        case ..<0.75:
            // Red → orange/yellow
            let s = (t - 0.5) / 0.25
            r = 1
            g = s * 0.7
            b = 0
        default:
            // Orange/yellow → white
            let s = (t - 0.75) / 0.25
            r = 1
            g = 0.7 + s * 0.3
            b = s
        }
        return Color(red: r, green: g, blue: b)
    }
}
