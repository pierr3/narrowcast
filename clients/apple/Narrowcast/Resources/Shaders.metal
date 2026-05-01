#include <metal_stdlib>
using namespace metal;

// Vertex coords arrive in normalized view space [0,1]x[0,1] with origin at
// bottom-left. Vertex shader maps to NDC.

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
