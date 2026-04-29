// Package dsp provides signal processing primitives for SDR.
package dsp

import (
	"math"
	"math/cmplx"
)

// FIRFilter applies a complex FIR filter to IQ data.
type FIRFilter struct {
	taps    []complex128
	history []complex128
	pos     int
}

// NewLowPassFIR designs a low-pass FIR filter using a Hamming window.
// cutoffHz is the cutoff frequency, sampleRate is the input sample rate.
func NewLowPassFIR(cutoffHz, sampleRate float64, numTaps int) []float64 {
	if numTaps%2 == 0 {
		numTaps++
	}
	taps := make([]float64, numTaps)
	mid := numTaps / 2
	fc := cutoffHz / sampleRate

	for i := 0; i < numTaps; i++ {
		n := float64(i - mid)
		// sinc
		if i == mid {
			taps[i] = 2 * fc
		} else {
			taps[i] = math.Sin(2*math.Pi*fc*n) / (math.Pi * n)
		}
		// Hamming window
		taps[i] *= 0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/float64(numTaps-1))
	}

	// Normalize
	var sum float64
	for _, t := range taps {
		sum += t
	}
	for i := range taps {
		taps[i] /= sum
	}
	return taps
}

// XlatingFilter performs frequency translation + decimation in one step.
// It shifts the desired signal to baseband and applies a low-pass FIR filter,
// then decimates by the given factor.
type XlatingFilter struct {
	taps       []complex128 // frequency-shifted FIR taps
	history    []complex128
	pos        int
	decimation int
	phase      complex128
	phaseStep  complex128
}

// NewXlatingFilter creates a frequency-translating decimating filter.
// offsetHz is the frequency offset from center to shift to baseband.
// lpfTaps are the real-valued low-pass filter taps.
// decimation is the output decimation factor.
// sampleRate is the input sample rate.
func NewXlatingFilter(offsetHz float64, lpfTaps []float64, decimation int, sampleRate float64) *XlatingFilter {
	ntaps := len(lpfTaps)
	ctaps := make([]complex128, ntaps)

	// Shift the LPF taps to create a bandpass centered on offsetHz
	for i, t := range lpfTaps {
		angle := 2 * math.Pi * offsetHz * float64(i) / sampleRate
		ctaps[i] = complex(t, 0) * cmplx.Rect(1, angle)
	}

	// Phase derotator step to shift back to baseband after decimation
	w := -2 * math.Pi * offsetHz / sampleRate
	phaseStep := cmplx.Rect(1, w*float64(decimation))

	return &XlatingFilter{
		taps:       ctaps,
		history:    make([]complex128, ntaps),
		decimation: decimation,
		phase:      complex(1, 0),
		phaseStep:  phaseStep,
	}
}

// Process applies the xlating filter to input IQ samples and returns decimated output.
func (f *XlatingFilter) Process(input []complex128) []complex128 {
	outLen := len(input) / f.decimation
	output := make([]complex128, 0, outLen)
	ntaps := len(f.taps)

	for i := 0; i < len(input); i++ {
		// Push sample into history
		f.history[f.pos] = input[i]
		f.pos = (f.pos + 1) % ntaps

		// Only compute output on decimation boundaries
		if (i+1)%f.decimation != 0 {
			continue
		}

		// FIR convolution
		var acc complex128
		idx := f.pos
		for j := 0; j < ntaps; j++ {
			acc += f.history[idx] * f.taps[j]
			idx = (idx + 1) % ntaps
		}

		// Derotate
		acc *= f.phase
		f.phase *= f.phaseStep
		// Normalize phase to prevent drift
		f.phase /= complex(cmplx.Abs(f.phase), 0)

		output = append(output, acc)
	}
	return output
}

// Reset zeroes the history buffer and resets the derotator phase. Call after
// a hardware retune or pipeline drop so stale samples don't bleed into the
// next FIR convolution as a transient.
func (f *XlatingFilter) Reset() {
	for i := range f.history {
		f.history[i] = 0
	}
	f.pos = 0
	f.phase = complex(1, 0)
}

// RealFIRFilter applies a real-valued FIR filter to audio samples with decimation.
type RealFIRFilter struct {
	taps    []float64
	history []float64
	pos     int
	decim   int
	count   int // counts input samples for decimation
}

// NewRealFIRDecimator creates a real-valued FIR low-pass filter with decimation.
// taps are the filter coefficients, decim is the decimation factor.
func NewRealFIRDecimator(taps []float64, decim int) *RealFIRFilter {
	return &RealFIRFilter{
		taps:    taps,
		history: make([]float64, len(taps)),
		decim:   decim,
	}
}

// Process filters and decimates real-valued samples.
func (f *RealFIRFilter) Process(input []float64) []float64 {
	output := make([]float64, 0, len(input)/f.decim+1)
	ntaps := len(f.taps)

	for _, s := range input {
		f.history[f.pos] = s
		f.pos = (f.pos + 1) % ntaps
		f.count++

		if f.count >= f.decim {
			f.count = 0
			var acc float64
			idx := f.pos
			for j := 0; j < ntaps; j++ {
				acc += f.history[idx] * f.taps[j]
				idx = (idx + 1) % ntaps
			}
			output = append(output, acc)
		}
	}
	return output
}

// Reset zeroes the history buffer and decimation phase counter.
func (f *RealFIRFilter) Reset() {
	for i := range f.history {
		f.history[i] = 0
	}
	f.pos = 0
	f.count = 0
}

// CU8ToComplex converts interleaved unsigned 8-bit IQ samples to complex128.
func CU8ToComplex(raw []byte) []complex128 {
	n := len(raw) / 2
	out := make([]complex128, n)
	for i := 0; i < n; i++ {
		re := (float64(raw[2*i]) - 127.5) / 128.0
		im := (float64(raw[2*i+1]) - 127.5) / 128.0
		out[i] = complex(re, im)
	}
	return out
}
