package dsp

import (
	"math"
	"math/cmplx"
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
		wn := cmplx.Rect(1, -2*math.Pi/float64(size))
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

// MagnitudeToU8 converts FFT output to magnitude bins in dBFS, mapped to 0-255.
// Performs FFT shift (DC center) and scales dBFS range [-120, 0] to [0, 255].
func MagnitudeToU8(fftOut []complex128) []byte {
	n := len(fftOut)
	bins := make([]byte, n)

	for i := 0; i < n; i++ {
		// FFT shift: swap halves so DC is in the center
		src := (i + n/2) % n
		mag := cmplx.Abs(fftOut[src]) / float64(n)
		if mag < 1e-12 {
			mag = 1e-12
		}
		dbfs := 20 * math.Log10(mag)
		// Map [-120, 0] dBFS to [0, 255]
		val := (dbfs + 120) * (255.0 / 120.0)
		if val < 0 {
			val = 0
		}
		if val > 255 {
			val = 255
		}
		bins[i] = byte(val)
	}
	return bins
}

// HannWindow applies a Hann window to complex samples in-place.
func HannWindow(data []complex128) {
	n := float64(len(data))
	for i := range data {
		w := 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/n))
		data[i] *= complex(w, 0)
	}
}
