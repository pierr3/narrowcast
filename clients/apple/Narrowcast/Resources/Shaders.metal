#include <metal_stdlib>
using namespace metal;

// Vertex coords arrive in normalized view space [0,1]x[0,1] with origin at
// bottom-left. Vertex shaders map to NDC.

struct Vertex {
    float2 pos;
};

struct Varyings {
    float4 position [[position]];
};

vertex Varyings spectrum_vertex(uint vid [[vertex_id]],
                                constant Vertex *verts [[buffer(0)]]) {
    Varyings out;
    float2 p = verts[vid].pos;
    out.position = float4(p.x * 2.0 - 1.0, p.y * 2.0 - 1.0, 0.0, 1.0);
    return out;
}

struct LineUniform {
    float4 color;
};

fragment float4 line_fragment(Varyings in [[stage_in]],
                              constant LineUniform &u [[buffer(0)]]) {
    return u.color;
}

// --- Spectrum bars ---
//
// Bars and peak caps carry a colour per vertex so the whole spectrum is one
// draw call; level-dependent tinting and the highlighted centre bar would each
// otherwise need their own.
//
// `pad` exists so `color` starts at offset 16 in both languages. Metal aligns
// float4 to 16 bytes and would insert the same padding implicitly — spelling it
// out keeps the Swift struct provably identical.
struct BarVertex {
    float2 pos;
    float2 pad;
    float4 color;
};

struct BarVaryings {
    float4 position [[position]];
    float4 color;
};

vertex BarVaryings bar_vertex(uint vid [[vertex_id]],
                              constant BarVertex *verts [[buffer(0)]]) {
    BarVaryings out;
    float2 p = verts[vid].pos;
    out.position = float4(p.x * 2.0 - 1.0, p.y * 2.0 - 1.0, 0.0, 1.0);
    out.color = verts[vid].color;
    return out;
}

fragment float4 bar_fragment(BarVaryings in [[stage_in]]) {
    return in.color;
}
