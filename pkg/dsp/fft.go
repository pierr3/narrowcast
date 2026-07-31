package dsp

import (
	"math"
)

// FFT computes a radix-2 Cooley-Tukey FFT in-place.
// Input length must be a power of 2.
func FFT(data []complex128) {
	n := len(data)
	if n <= 1 {
		return
	}

	// Bit-reversal permutation
	j := 0
	for i := 1; i < n; i++ {
		bit := n >> 1
		for j&bit != 0 {
			j ^= bit
			bit >>= 1
		}
		j ^= bit
		if i < j {
			data[i], data[j] = data[j], data[i]
		}
	}

	// Butterfly stages
	for size := 2; size <= n; size <<= 1 {
		half := size / 2
		theta := -2 * math.Pi / float64(size)
		wn := complex(math.Cos(theta), math.Sin(theta))
		for start := 0; start < n; start += size {
			w := complex(1, 0)
			for k := 0; k < half; k++ {
				u := data[start+k]
				v := data[start+k+half] * w
				data[start+k] = u + v
				data[start+k+half] = u - v
				w *= wn
			}
		}
	}
}

// HannWindow is a precomputed Hann window. Building the coefficients once per
// DSP chain rather than per frame removes a cosine per bin per frame.
type HannWindow struct {
	w []float64
}

// NewHannWindow builds a Hann window of length n.
func NewHannWindow(n int) *HannWindow {
	w := make([]float64, n)
	for i := range w {
		w[i] = 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(n)))
	}
	return &HannWindow{w: w}
}

// Apply windows data in-place. A size mismatch would be a chain-construction
// bug, so it windows the overlapping prefix rather than panicking mid-stream.
func (h *HannWindow) Apply(data []complex128) {
	n := min(len(data), len(h.w))
	for i := 0; i < n; i++ {
		data[i] *= complex(h.w[i], 0)
	}
}

// MagnitudeToBins converts FFT output into display bins in dst, growing dst if
// needed, and returns the filled slice.
//
// It FFT-shifts (DC to the center), converts to dBFS, maps [-120, 0] dBFS onto
// [0, 255], and max-pools down to outBins. outBins <= 0 or >= len(fftOut) gives
// one output bin per FFT bin.
//
// Max-pooling rather than averaging is deliberate: a narrow carrier occupying a
// single FFT bin must survive into the displayed bin, and averaging would bury
// it in the surrounding noise floor. Pooling server-side also keeps the FFT
// datagram small — 1024 bins at 20 fps is ~20 KB/s, which alone exceeds the
// whole uplink budget, where 256 bins at 10 fps is ~2.6 KB/s.
func MagnitudeToBins(dst []byte, fftOut []complex128, outBins int) []byte {
	n := len(fftOut)
	if n == 0 {
		return dst[:0]
	}
	if outBins <= 0 || outBins > n {
		outBins = n
	}
	if cap(dst) < outBins {
		dst = make([]byte, outBins)
	}
	dst = dst[:outBins]

	// Work in power (re²+im²) so the group scan needs no sqrt, then take one
	// logarithm per output bin instead of per FFT bin:
	//   dBFS = 20·log10(sqrt(p)/n) = 10·log10(p) − 20·log10(n)
	const minPower = 1e-24 // matches the previous 1e-12 magnitude floor
	offsetDb := -20 * math.Log10(float64(n))
	half := n / 2

	for k := 0; k < outBins; k++ {
		// Integer-exact group boundaries, so a bin count that doesn't divide n
		// spreads the remainder instead of dropping bins off the end.
		start := k * n / outBins
		end := (k + 1) * n / outBins
		if end <= start {
			end = start + 1
		}

		maxP := 0.0
		for i := start; i < end; i++ {
			// FFT shift, without a modulo per bin.
			src := i + half
			if src >= n {
				src -= n
			}
			s := fftOut[src]
			re, im := real(s), imag(s)
			if p := re*re + im*im; p > maxP {
				maxP = p
			}
		}
		if maxP < minPower {
			maxP = minPower
		}

		// Map [-120, 0] dBFS onto [0, 255], rounding rather than truncating.
		// Truncation costs half an LSB across the whole range and, worse, turns
		// a full-scale bin into 254 whenever the logarithms land a hair below
		// zero — which they do, since the dB offset is now applied as a
		// subtraction rather than folded into the magnitude.
		v := int((10*math.Log10(maxP)+offsetDb+120)*(255.0/120.0) + 0.5)
		switch {
		case v < 0:
			v = 0
		case v > 255:
			v = 255
		}
		dst[k] = byte(v)
	}
	return dst
}
