import SwiftUI
import MetalKit

#if canImport(UIKit)
import UIKit
#endif

// SpectrumView wraps a Metal-backed MTKView in SwiftUI.
//
// It draws a bar chart rather than a 1024-point line. On a phone the old line
// had ~3 FFT bins per point of width — detail the screen physically cannot
// resolve — and it redrew at 60 fps on the main thread regardless of the FFT
// rate, which is main-thread time competing with SwiftUI. Bars are both cheaper
// and more legible: one bar covers a channel-sized slice of spectrum, so "which
// block is busy" is readable at a glance.
struct SpectrumView: UIViewRepresentable {

    let store: SpectrumStore

    func makeUIView(context: Context) -> MetalSpectrumView {
        MetalSpectrumView(store: store)
    }

    func updateUIView(_ v: MetalSpectrumView, context: Context) {
        v.store = store
    }

    static func dismantleUIView(_ v: MetalSpectrumView, coordinator: ()) {
        v.detach()
    }
}

final class MetalSpectrumView: MTKView, MTKViewDelegate {

    var store: SpectrumStore {
        didSet {
            guard oldValue !== store else { return }
            oldValue.setFrameHandler(nil)
            attachIfVisible()
        }
    }

    /// Target width of one bar plus its gap, in points. ~6 pt gives about 60
    /// bars on a phone, which at a 960 kHz span is ~16 kHz per bar — one
    /// narrowband channel each.
    private let barPitch: CGFloat = 6
    /// Fraction of each slot left empty between bars.
    private let gapFraction: Float = 0.22
    /// Headroom above the tallest bar so it doesn't touch the edge labels.
    private let yScale: Float = 0.92
    /// Height of the peak-hold cap, in normalized view units.
    private let peakCapHeight: Float = 0.025

    private let commandQueue: MTLCommandQueue
    private let barPipeline: MTLRenderPipelineState
    private let linePipeline: MTLRenderPipelineState

    // Bars and peak caps share one buffer and one draw call: 6 vertices per
    // quad, two quads per bar.
    private static let maxBars = 256
    private static let vertsPerBar = 12
    private var barBuffer: MTLBuffer
    private var barVertexCount = 0

    // Static centre-frequency marker.
    private var decorBuffer: MTLBuffer

    // Scratch for the store read, sized to the current bar count.
    private var levels: [Float] = []
    private var peaks: [Float] = []

    /// Matches `BarVertex` in Shaders.metal. `pad` keeps `color` at offset 16.
    private struct BarVertex {
        var pos: SIMD2<Float>
        var pad: SIMD2<Float> = .zero
        var color: SIMD4<Float>
    }

    init(store: SpectrumStore) {
        self.store = store

        guard let dev = MTLCreateSystemDefaultDevice(),
              let q = dev.makeCommandQueue() else {
            fatalError("Metal not available")
        }
        guard let lib = dev.makeDefaultLibrary() else {
            fatalError("Metal default library missing — Shaders.metal not compiled?")
        }
        guard
            let barVfn = lib.makeFunction(name: "bar_vertex"),
            let barFfn = lib.makeFunction(name: "bar_fragment"),
            let lineVfn = lib.makeFunction(name: "spectrum_vertex"),
            let lineFfn = lib.makeFunction(name: "line_fragment")
        else {
            fatalError("Spectrum shaders missing from Metal library")
        }

        self.commandQueue = q

        let format = MTLPixelFormat.bgra8Unorm
        // No MSAA. Bars are axis-aligned rectangles with nothing to
        // anti-alias, unlike the line strip this replaced.
        func makePipeline(_ vfn: MTLFunction, _ ffn: MTLFunction) -> MTLRenderPipelineState {
            let desc = MTLRenderPipelineDescriptor()
            desc.vertexFunction = vfn
            desc.fragmentFunction = ffn
            desc.rasterSampleCount = 1
            desc.colorAttachments[0].pixelFormat = format
            desc.colorAttachments[0].isBlendingEnabled = true
            desc.colorAttachments[0].rgbBlendOperation = .add
            desc.colorAttachments[0].alphaBlendOperation = .add
            desc.colorAttachments[0].sourceRGBBlendFactor = .sourceAlpha
            desc.colorAttachments[0].sourceAlphaBlendFactor = .one
            desc.colorAttachments[0].destinationRGBBlendFactor = .oneMinusSourceAlpha
            desc.colorAttachments[0].destinationAlphaBlendFactor = .oneMinusSourceAlpha
            return (try? dev.makeRenderPipelineState(descriptor: desc))!
        }
        self.barPipeline = makePipeline(barVfn, barFfn)
        self.linePipeline = makePipeline(lineVfn, lineFfn)

        self.barBuffer = dev.makeBuffer(
            length: Self.maxBars * Self.vertsPerBar * MemoryLayout<BarVertex>.stride,
            options: .storageModeShared)!
        self.decorBuffer = dev.makeBuffer(
            length: 2 * MemoryLayout<SIMD2<Float>>.stride,
            options: .storageModeShared)!

        super.init(frame: .zero, device: dev)

        self.colorPixelFormat = format
        self.sampleCount = 1
        // Draw on demand. The FFT arrives at 1-10 fps, so a 60 fps display link
        // was up to 60× the necessary main-thread work — and it kept running
        // when the view was off-screen.
        self.isPaused = true
        self.enableSetNeedsDisplay = true
        self.framebufferOnly = true
        // Transparent so the surrounding card colour shows through.
        self.clearColor = MTLClearColor(red: 0, green: 0, blue: 0, alpha: 0)
        self.layer.isOpaque = false
        self.delegate = self

        buildDecor()
    }

    required init(coder: NSCoder) {
        fatalError("init(coder:) not used")
    }

    deinit {
        store.setFrameHandler(nil)
    }

    /// Detach from the store so a torn-down view stops being woken by frames.
    func detach() {
        store.setFrameHandler(nil)
    }

    // Redraw only while in the window hierarchy: navigating away should cost
    // nothing at all.
    override func didMoveToWindow() {
        super.didMoveToWindow()
        attachIfVisible()
    }

    private func attachIfVisible() {
        guard window != nil else {
            store.setFrameHandler(nil)
            return
        }
        store.setFrameHandler { [weak self] in
            // setNeedsDisplay is main-thread only; the store signals off-main.
            DispatchQueue.main.async { self?.setNeedsDisplay() }
        }
        // Paint once on appearance rather than waiting for the next FFT frame,
        // which under heavy loss can be a second away.
        setNeedsDisplay()
    }

    // MARK: - MTKViewDelegate

    func mtkView(_ view: MTKView, drawableSizeWillChange size: CGSize) {
        setNeedsDisplay()
    }

    func draw(in view: MTKView) {
        guard
            let drawable = currentDrawable,
            let descriptor = currentRenderPassDescriptor,
            let cmd = commandQueue.makeCommandBuffer(),
            let enc = cmd.makeRenderCommandEncoder(descriptor: descriptor)
        else { return }

        if buildBars() {
            enc.setRenderPipelineState(barPipeline)
            enc.setVertexBuffer(barBuffer, offset: 0, index: 0)
            enc.drawPrimitives(type: .triangle, vertexStart: 0, vertexCount: barVertexCount)
        }

        // Centre-frequency marker. The squelch line the old view drew is gone:
        // levels are auto-ranged now (see SpectrumStore), so a threshold in
        // absolute dBm no longer corresponds to a height on this axis. Squelch
        // is still shown numerically and as the S-meter's tint threshold.
        enc.setRenderPipelineState(linePipeline)
        enc.setVertexBuffer(decorBuffer, offset: 0, index: 0)
        var centerColor = SIMD4<Float>(0.55, 0.60, 0.70, 0.45)
        enc.setFragmentBytes(&centerColor, length: MemoryLayout<SIMD4<Float>>.size, index: 0)
        enc.drawPrimitives(type: .line, vertexStart: 0, vertexCount: 2)

        enc.endEncoding()
        cmd.present(drawable)
        cmd.commit()
    }

    // MARK: - Geometry

    /// Fill barBuffer from the store. Returns false when there is nothing to draw.
    private func buildBars() -> Bool {
        let widthPoints = bounds.width
        guard widthPoints > 0 else { return false }

        let count = min(max(Int(widthPoints / barPitch), 8), Self.maxBars)
        if levels.count != count {
            levels = Array(repeating: 0, count: count)
            peaks = Array(repeating: 0, count: count)
        }
        guard store.readBars(level: &levels, peak: &peaks) else { return false }

        let slot = 1 / Float(count)
        let gap = slot * gapFraction
        let center = count / 2

        let ptr = barBuffer.contents().bindMemory(
            to: BarVertex.self, capacity: Self.maxBars * Self.vertsPerBar)
        var v = 0

        func quad(x0: Float, x1: Float, y0: Float, y1: Float, color: SIMD4<Float>) {
            let corners = [
                SIMD2<Float>(x0, y0), SIMD2<Float>(x1, y0), SIMD2<Float>(x1, y1),
                SIMD2<Float>(x0, y0), SIMD2<Float>(x1, y1), SIMD2<Float>(x0, y1),
            ]
            for c in corners {
                ptr[v] = BarVertex(pos: c, color: color)
                v += 1
            }
        }

        for i in 0..<count {
            let x0 = Float(i) * slot + gap * 0.5
            let x1 = Float(i + 1) * slot - gap * 0.5
            let level = levels[i]
            let peak = peaks[i]

            // Colour by level so activity reads without checking the height,
            // with the tuned (centre) bar tinted so it's findable at a glance.
            let color = i == center
                ? SIMD4<Float>(0.95, 0.55, 0.20, 0.95)
                : mix(low: SIMD4<Float>(0.20, 0.28, 0.42, 0.80),
                      high: SIMD4<Float>(0.10, 0.55, 0.95, 1.00),
                      t: level)

            // A floor of one cap-height keeps empty bars visible as a baseline
            // rather than vanishing entirely.
            let top = max(level * yScale, peakCapHeight * 0.5)
            quad(x0: x0, x1: x1, y0: 0, y1: top, color: color)

            // Peak cap, only when it has separated from the bar itself.
            let capBottom = peak * yScale - peakCapHeight
            if capBottom > top {
                quad(x0: x0, x1: x1,
                     y0: capBottom, y1: peak * yScale,
                     color: SIMD4<Float>(0.75, 0.80, 0.90, 0.55))
            }
        }

        barVertexCount = v
        return v > 0
    }

    private func mix(low: SIMD4<Float>, high: SIMD4<Float>, t: Float) -> SIMD4<Float> {
        let k = min(max(t, 0), 1)
        return low + (high - low) * k
    }

    private func buildDecor() {
        let ptr = decorBuffer.contents().bindMemory(to: SIMD2<Float>.self, capacity: 2)
        ptr[0] = SIMD2<Float>(0.5, 0)
        ptr[1] = SIMD2<Float>(0.5, yScale)
    }
}
