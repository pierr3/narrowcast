// Package dsp provides signal processing primitives for SDR.
//
// Buffer ownership convention: every stage that produces a new slice
// (XlatingFilter, RealFIRFilter, the demodulators) returns a slice backed by a
// buffer it owns and reuses. The result stays valid until the next call to that
// same stage, and callers must not retain it. This is what keeps the pipeline
// from generating tens of megabytes per second of garbage on a Raspberry Pi,
// where that GC churn is a measurable slice of the thermal budget.
package dsp

import (
	"math"
	"math/cmplx"
)

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

// overlapSave carries the ntaps-1 trailing samples a block-based FIR needs from
// the previous block, and tracks decimation phase across blocks. Shared by the
// complex and real filters so the bookkeeping exists once.
//
// It replaces the circular history buffer these filters used to keep. A ring
// buffer costs an integer modulo per tap per output — 145 taps at 32 k
// outputs/s was ~4.6 M integer divisions per second inside the hottest loop in
// the program. Overlap-save trades all of that for one memmove per block plus a
// straight-line inner loop over contiguous memory.
type overlapSave[T float64 | complex128] struct {
	carry []T // ntaps-1 samples from the end of the previous block
	work  []T // carry ++ current block, reused across calls
	decim int
	phase int // inputs consumed since the last emitted output
}

func newOverlapSave[T float64 | complex128](ntaps, decim int) overlapSave[T] {
	if decim < 1 {
		decim = 1
	}
	if ntaps < 1 {
		ntaps = 1
	}
	return overlapSave[T]{
		carry: make([]T, ntaps-1),
		decim: decim,
	}
}

// window returns carry ++ in. Output sample i of the block is the convolution
// of window[i:i+ntaps] with the taps, so output indices line up with input
// indices and the decimation phase needs no correction.
func (o *overlapSave[T]) window(in []T) []T {
	need := len(o.carry) + len(in)
	if cap(o.work) < need {
		o.work = make([]T, need)
	}
	o.work = o.work[:need]
	copy(o.work, o.carry)
	copy(o.work[len(o.carry):], in)
	return o.work
}

// save retains the trailing ntaps-1 samples for the next block. work must be
// the slice returned by window, which always contains the whole carry and so is
// never shorter than it.
func (o *overlapSave[T]) save(work []T) {
	if n := len(o.carry); n > 0 {
		copy(o.carry, work[len(work)-n:])
	}
}

// emit advances the decimation phase by one input sample and reports whether an
// output falls on it.
func (o *overlapSave[T]) emit() bool {
	o.phase++
	if o.phase < o.decim {
		return false
	}
	o.phase = 0
	return true
}

// outputCap is a sufficient capacity for one block's worth of outputs.
func (o *overlapSave[T]) outputCap(inLen int) int {
	return inLen/o.decim + 1
}

func (o *overlapSave[T]) reset() {
	var zero T
	for i := range o.carry {
		o.carry[i] = zero
	}
	o.phase = 0
}

// XlatingFilter performs frequency translation + decimation in one step.
// It shifts the desired signal to baseband, applies a low-pass FIR filter, and
// decimates by the given factor.
type XlatingFilter struct {
	// With offsetHz == 0 — the only case this server uses, since retuning
	// happens in hardware — the taps stay real and no derotation is needed.
	// That halves the multiplies per tap (2 instead of 4) and drops a
	// cmplx.Abs plus two divisions per output sample.
	realTaps []float64
	cplxTaps []complex128

	os  overlapSave[complex128]
	out []complex128

	rotPhase complex128
	rotStep  complex128
}

// NewXlatingFilter creates a frequency-translating decimating filter.
// offsetHz is the frequency offset from center to shift to baseband: a positive
// offsetHz selects the band above center. lpfTaps are the real-valued low-pass
// filter taps, decimation is the output decimation factor, and sampleRate is
// the input sample rate.
func NewXlatingFilter(offsetHz float64, lpfTaps []float64, decimation int, sampleRate float64) *XlatingFilter {
	f := &XlatingFilter{
		os:       newOverlapSave[complex128](len(lpfTaps), decimation),
		rotPhase: complex(1, 0),
	}

	if offsetHz == 0 {
		f.realTaps = lpfTaps
		return f
	}

	// Shift the LPF taps to create a bandpass centered on offsetHz.
	//
	// The convolution below pairs taps[0] with the oldest sample, so the tap
	// sequence is traversed in the same direction as time and the response of
	// h[j] = g[j]·e^(-jωj) sits at +ω. (The previous +jω here put the passband
	// at -offsetHz — invisible in practice, since this server always retunes in
	// hardware and passes offsetHz = 0, but wrong for any future caller.)
	ctaps := make([]complex128, len(lpfTaps))
	for i, t := range lpfTaps {
		angle := 2 * math.Pi * offsetHz * float64(i) / sampleRate
		ctaps[i] = complex(t, 0) * cmplx.Rect(1, -angle)
	}
	f.cplxTaps = ctaps
	// Phase derotator step, shifting back to baseband after decimation.
	w := -2 * math.Pi * offsetHz / sampleRate
	f.rotStep = cmplx.Rect(1, w*float64(decimation))
	return f
}

// Process applies the xlating filter to input IQ samples and returns decimated
// output. The returned slice is owned by the filter and valid until the next
// call (see the package comment).
func (f *XlatingFilter) Process(input []complex128) []complex128 {
	work := f.os.window(input)

	if need := f.os.outputCap(len(input)); cap(f.out) < need {
		f.out = make([]complex128, 0, need)
	}
	out := f.out[:0]

	if f.realTaps != nil {
		taps := f.realTaps
		ntaps := len(taps)
		for i := 0; i < len(input); i++ {
			if !f.os.emit() {
				continue
			}
			w := work[i : i+ntaps]
			var re, im float64
			for j := 0; j < ntaps; j++ {
				s := w[j]
				t := taps[j]
				re += real(s) * t
				im += imag(s) * t
			}
			out = append(out, complex(re, im))
		}
	} else {
		taps := f.cplxTaps
		ntaps := len(taps)
		for i := 0; i < len(input); i++ {
			if !f.os.emit() {
				continue
			}
			w := work[i : i+ntaps]
			var acc complex128
			for j := 0; j < ntaps; j++ {
				acc += w[j] * taps[j]
			}
			// Derotate, renormalizing to stop magnitude drift.
			acc *= f.rotPhase
			f.rotPhase *= f.rotStep
			f.rotPhase /= complex(cmplx.Abs(f.rotPhase), 0)
			out = append(out, acc)
		}
	}

	f.os.save(work)
	f.out = out
	return out
}

// Reset clears filter history and the derotator phase. Call after a hardware
// retune or a pipeline drop so stale samples don't bleed into the next block as
// a transient.
func (f *XlatingFilter) Reset() {
	f.os.reset()
	f.rotPhase = complex(1, 0)
}

// RealFIRFilter applies a real-valued FIR filter to audio samples with
// decimation. decim of 1 filters without decimating.
type RealFIRFilter struct {
	taps []float64
	os   overlapSave[float64]
	out  []float64
}

// NewRealFIRDecimator creates a real-valued FIR filter with decimation.
// taps are the filter coefficients, decim is the decimation factor.
func NewRealFIRDecimator(taps []float64, decim int) *RealFIRFilter {
	return &RealFIRFilter{
		taps: taps,
		os:   newOverlapSave[float64](len(taps), decim),
	}
}

// Process filters and decimates real-valued samples. The returned slice is
// owned by the filter and valid until the next call.
func (f *RealFIRFilter) Process(input []float64) []float64 {
	work := f.os.window(input)

	if need := f.os.outputCap(len(input)); cap(f.out) < need {
		f.out = make([]float64, 0, need)
	}
	out := f.out[:0]

	taps := f.taps
	ntaps := len(taps)
	for i := 0; i < len(input); i++ {
		if !f.os.emit() {
			continue
		}
		w := work[i : i+ntaps]
		var acc float64
		for j := 0; j < ntaps; j++ {
			acc += w[j] * taps[j]
		}
		out = append(out, acc)
	}

	f.os.save(work)
	f.out = out
	return out
}

// Reset zeroes filter history and the decimation phase.
func (f *RealFIRFilter) Reset() {
	f.os.reset()
}

// cu8Lut maps a raw CU8 byte to its normalized float value. A 2 KiB table
// removes an int→float conversion, a subtraction and a division from the
// highest-rate loop in the program (2 × sampleRate lookups per second).
var cu8Lut [256]float64

func init() {
	for i := range cu8Lut {
		cu8Lut[i] = (float64(i) - 127.5) / 128.0
	}
}

// CU8ToComplexInto converts interleaved unsigned 8-bit IQ samples into dst,
// growing it if needed, and returns the filled slice. Pass the previous return
// value back in to reuse the allocation.
func CU8ToComplexInto(dst []complex128, raw []byte) []complex128 {
	n := len(raw) / 2
	if cap(dst) < n {
		dst = make([]complex128, n)
	}
	dst = dst[:n]
	for i := 0; i < n; i++ {
		dst[i] = complex(cu8Lut[raw[2*i]], cu8Lut[raw[2*i+1]])
	}
	return dst
}

// CU8ToComplex converts interleaved unsigned 8-bit IQ samples to complex128,
// allocating a fresh slice. Prefer CU8ToComplexInto on hot paths.
func CU8ToComplex(raw []byte) []complex128 {
	return CU8ToComplexInto(nil, raw)
}
