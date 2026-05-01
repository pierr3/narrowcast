import SwiftUI
import MetalKit

#if canImport(UIKit)
import UIKit
#endif

// SpectrumView wraps a Metal-backed MTKView in SwiftUI. The view runs at
// 60 fps regardless of the FFT frame rate from the server (which adapts
// from 20 fps down to 1 fps under loss). Smoothing + peak hold come from
// SpectrumStore; this layer is purely presentation.
struct SpectrumView: UIViewRepresentable {

    let store: SpectrumStore
    let squelchDb: Float    // dBm, mapped same -120..0 scale as FFT bins

    func makeUIView(context: Context) -> MetalSpectrumView {
        let v = MetalSpectrumView()
        v.store = store
        v.squelchDb = squelchDb
        return v
    }

    func updateUIView(_ v: MetalSpectrumView, context: Context) {
        v.store = store
        v.squelchDb = squelchDb
    }
}

final class MetalSpectrumView: MTKView, MTKViewDelegate {

    var store: SpectrumStore?
    var squelchDb: Float = -80

    private let commandQueue: MTLCommandQueue
    private let fillPipeline: MTLRenderPipelineState
    private let linePipeline: MTLRenderPipelineState

    // Triangle-strip vertex buffer for the filled spectrum body. Pre-sized
    // for 1024 bins * 2 verts (baseline + top) — server's default FFT size.
    private var fillVertexBuffer: MTLBuffer
    private var fillVertexCount: Int = 0

    // Line strip on top of the fill — the bright cyan stroke marking the
    // spectrum curve. One vertex per bin.
    private var topLineBuffer: MTLBuffer
    private var topLineCount: Int = 0

    // Peak-hold trace.
    private var peakLineBuffer: MTLBuffer
    private var peakLineCount: Int = 0

    // Static decoration: squelch threshold + center crosshair. Rebuilt
    // when squelchDb changes.
    private var decorBuffer: MTLBuffer
    private var decorSegments: [(start: Int, count: Int, color: SIMD4<Float>)] = []
    private var lastSquelchDb: Float = .nan

    init() {
        guard let dev = MTLCreateSystemDefaultDevice(),
              let q = dev.makeCommandQueue() else {
            fatalError("Metal not available")
        }
        guard let lib = dev.makeDefaultLibrary() else {
            fatalError("Metal default library missing — Shaders.metal not compiled?")
        }
        guard
            let vfn = lib.makeFunction(name: "spectrum_vertex"),
            let ffn = lib.makeFunction(name: "spectrum_fill_fragment"),
            let lfn = lib.makeFunction(name: "line_fragment")
        else {
            fatalError("Spectrum shaders missing from Metal library")
        }

        self.commandQueue = q

        let format = MTLPixelFormat.bgra8Unorm

        let fillDesc = MTLRenderPipelineDescriptor()
        fillDesc.vertexFunction = vfn
        fillDesc.fragmentFunction = ffn
        fillDesc.colorAttachments[0].pixelFormat = format
        fillDesc.colorAttachments[0].isBlendingEnabled = true
        fillDesc.colorAttachments[0].rgbBlendOperation = .add
        fillDesc.colorAttachments[0].alphaBlendOperation = .add
        fillDesc.colorAttachments[0].sourceRGBBlendFactor = .one
        fillDesc.colorAttachments[0].sourceAlphaBlendFactor = .one
        fillDesc.colorAttachments[0].destinationRGBBlendFactor = .oneMinusSourceAlpha
        fillDesc.colorAttachments[0].destinationAlphaBlendFactor = .oneMinusSourceAlpha
        self.fillPipeline = (try? dev.makeRenderPipelineState(descriptor: fillDesc))!

        let lineDesc = MTLRenderPipelineDescriptor()
        lineDesc.vertexFunction = vfn
        lineDesc.fragmentFunction = lfn
        lineDesc.colorAttachments[0].pixelFormat = format
        lineDesc.colorAttachments[0].isBlendingEnabled = true
        lineDesc.colorAttachments[0].rgbBlendOperation = .add
        lineDesc.colorAttachments[0].alphaBlendOperation = .add
        lineDesc.colorAttachments[0].sourceRGBBlendFactor = .sourceAlpha
        lineDesc.colorAttachments[0].sourceAlphaBlendFactor = .one
        lineDesc.colorAttachments[0].destinationRGBBlendFactor = .oneMinusSourceAlpha
        lineDesc.colorAttachments[0].destinationAlphaBlendFactor = .oneMinusSourceAlpha
        self.linePipeline = (try? dev.makeRenderPipelineState(descriptor: lineDesc))!

        // 4096 bins * 2 verts * float2 = headroom for any FFT size we ship.
        let cap = 4096 * 2 * MemoryLayout<SIMD2<Float>>.stride
        self.fillVertexBuffer = dev.makeBuffer(length: cap, options: .storageModeShared)!
        self.topLineBuffer = dev.makeBuffer(length: 4096 * MemoryLayout<SIMD2<Float>>.stride, options: .storageModeShared)!
        self.peakLineBuffer = dev.makeBuffer(length: 4096 * MemoryLayout<SIMD2<Float>>.stride, options: .storageModeShared)!
        self.decorBuffer = dev.makeBuffer(length: 64 * MemoryLayout<SIMD2<Float>>.stride, options: .storageModeShared)!

        super.init(frame: .zero, device: dev)

        self.colorPixelFormat = format
        self.preferredFramesPerSecond = 60
        self.isPaused = false
        self.enableSetNeedsDisplay = false
        self.framebufferOnly = true
        // Transparent so the surrounding card colour shows through.
        self.clearColor = MTLClearColor(red: 0, green: 0, blue: 0, alpha: 0)
        self.layer.isOpaque = false
        self.delegate = self
    }

    required init(coder: NSCoder) {
        fatalError("init(coder:) not used")
    }

    // MARK: - MTKViewDelegate

    func mtkView(_ view: MTKView, drawableSizeWillChange size: CGSize) {}

    func draw(in view: MTKView) {
        guard
            let store,
            let drawable = currentDrawable,
            let descriptor = currentRenderPassDescriptor,
            let cmd = commandQueue.makeCommandBuffer(),
            let enc = cmd.makeRenderCommandEncoder(descriptor: descriptor)
        else { return }

        let snap = store.snapshot()
        let n = snap.binCount
        if n < 2 {
            enc.endEncoding()
            cmd.present(drawable)
            cmd.commit()
            return
        }

        // Margin at the top so the spectrum curve doesn't kiss the edge
        // labels. yScale < 1.0 leaves a transparent strip up top.
        let yScale: Float = 0.92

        // --- Fill triangle strip ---
        do {
            let ptr = fillVertexBuffer.contents().bindMemory(to: SIMD2<Float>.self, capacity: n * 2)
            for i in 0..<n {
                let x = Float(i) / Float(n - 1)
                let y = snap.smooth[i] * yScale
                ptr[i * 2 + 0] = SIMD2<Float>(x, 0)
                ptr[i * 2 + 1] = SIMD2<Float>(x, y)
            }
            fillVertexCount = n * 2
        }

        // --- Top edge line strip ---
        do {
            let ptr = topLineBuffer.contents().bindMemory(to: SIMD2<Float>.self, capacity: n)
            for i in 0..<n {
                let x = Float(i) / Float(n - 1)
                let y = snap.smooth[i] * yScale
                ptr[i] = SIMD2<Float>(x, y)
            }
            topLineCount = n
        }

        // --- Peak hold line ---
        do {
            let ptr = peakLineBuffer.contents().bindMemory(to: SIMD2<Float>.self, capacity: n)
            for i in 0..<n {
                let x = Float(i) / Float(n - 1)
                let y = snap.peak[i] * yScale
                ptr[i] = SIMD2<Float>(x, y)
            }
            peakLineCount = n
        }

        // --- Decor (squelch + center marker) — only rebuild on change ---
        if abs(squelchDb - lastSquelchDb) > 0.05 {
            rebuildDecor(yScale: yScale)
            lastSquelchDb = squelchDb
        }

        // --- Draw fill ---
        enc.setRenderPipelineState(fillPipeline)
        enc.setVertexBuffer(fillVertexBuffer, offset: 0, index: 0)
        enc.drawPrimitives(type: .triangleStrip, vertexStart: 0, vertexCount: fillVertexCount)

        // --- Draw peak hold (faint white) ---
        enc.setRenderPipelineState(linePipeline)
        enc.setVertexBuffer(peakLineBuffer, offset: 0, index: 0)
        var peakColor = SIMD4<Float>(1.0, 1.0, 1.0, 0.35)
        enc.setFragmentBytes(&peakColor, length: MemoryLayout<SIMD4<Float>>.size, index: 0)
        enc.drawPrimitives(type: .lineStrip, vertexStart: 0, vertexCount: peakLineCount)

        // --- Draw top edge (bright cyan) ---
        enc.setVertexBuffer(topLineBuffer, offset: 0, index: 0)
        var topColor = SIMD4<Float>(0.45, 0.85, 1.0, 1.0)
        enc.setFragmentBytes(&topColor, length: MemoryLayout<SIMD4<Float>>.size, index: 0)
        enc.drawPrimitives(type: .lineStrip, vertexStart: 0, vertexCount: topLineCount)

        // --- Draw decor segments ---
        enc.setVertexBuffer(decorBuffer, offset: 0, index: 0)
        for seg in decorSegments {
            var c = seg.color
            enc.setFragmentBytes(&c, length: MemoryLayout<SIMD4<Float>>.size, index: 0)
            enc.drawPrimitives(type: .line, vertexStart: seg.start, vertexCount: seg.count)
        }

        enc.endEncoding()
        cmd.present(drawable)
        cmd.commit()
    }

    private func rebuildDecor(yScale: Float) {
        // Map squelchDb (-120..0) onto 0..1 then scale into the curve area.
        let squelchY = max(0.0, min(1.0, (squelchDb + 120) / 120)) * yScale

        // Segment layout:
        //   0..N : squelch dashed line (10 dashes = 20 verts)
        //   N..N+2 : center crosshair (vertical, 2 verts)
        let dashCount = 12
        let dashVerts = dashCount * 2
        let totalVerts = dashVerts + 2
        let ptr = decorBuffer.contents().bindMemory(to: SIMD2<Float>.self, capacity: totalVerts)

        // Squelch dashed line
        for i in 0..<dashCount {
            let t0 = Float(i * 2) / Float(dashCount * 2)
            let t1 = Float(i * 2 + 1) / Float(dashCount * 2)
            ptr[i * 2 + 0] = SIMD2<Float>(t0, squelchY)
            ptr[i * 2 + 1] = SIMD2<Float>(t1, squelchY)
        }
        // Center crosshair
        ptr[dashVerts + 0] = SIMD2<Float>(0.5, 0)
        ptr[dashVerts + 1] = SIMD2<Float>(0.5, yScale)

        decorSegments = [
            // squelch, warm orange
            (start: 0, count: dashVerts, color: SIMD4<Float>(1.0, 0.55, 0.35, 0.9)),
            // crosshair, faint white
            (start: dashVerts, count: 2, color: SIMD4<Float>(1.0, 1.0, 1.0, 0.25)),
        ]
    }
}
