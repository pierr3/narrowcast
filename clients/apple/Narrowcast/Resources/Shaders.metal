#include <metal_stdlib>
using namespace metal;

// Vertex coords arrive in normalized view space [0,1]x[0,1] with origin at
// bottom-left. The vertex shader maps them to NDC. Fragment shader receives
// the original (u,v) for gradient and edge falloff.

struct Vertex {
    float2 pos;     // (x in 0..1, y in 0..1)
};

struct Varyings {
    float4 position [[position]];
    float2 uv;
};

vertex Varyings spectrum_vertex(uint vid [[vertex_id]],
                                constant Vertex *verts [[buffer(0)]]) {
    Varyings out;
    float2 p = verts[vid].pos;
    out.position = float4(p.x * 2.0 - 1.0, p.y * 2.0 - 1.0, 0.0, 1.0);
    out.uv = p;
    return out;
}

// Filled spectrum body: vertical gradient from deep blue at the baseline
// to bright cyan at the top, with the alpha rising as well so the fill
// is barely-there low and prominent at the spike tops.
fragment float4 spectrum_fill_fragment(Varyings in [[stage_in]]) {
    float t = clamp(in.uv.y, 0.0, 1.0);
    float3 lo = float3(0.04, 0.30, 0.55);
    float3 hi = float3(0.31, 0.76, 0.97);
    float3 c = mix(lo, hi, t);
    float a = mix(0.25, 0.85, t);
    return float4(c * a, a);
}

// Solid colour fragment for line-strip / line-segment passes. The colour
// is supplied as a uniform.
struct LineUniform {
    float4 color;
};

fragment float4 line_fragment(Varyings in [[stage_in]],
                              constant LineUniform &u [[buffer(0)]]) {
    return u.color;
}
