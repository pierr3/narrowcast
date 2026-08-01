package dsp

import (
	"math"
	"math/cmplx"
)

// FineTuner shifts a channel stream so a chosen carrier lands at DC, then
// applies a narrow low-pass around it.
//
// This runs on the *decimated* channel stream (48 kHz for AM), not the raw SDR
// stream, and that placement is the whole point. Centring at the SDR rate would
// mean giving the wideband channel filter complex taps — four multiplies per tap
// instead of two, on the single most expensive loop in the program, 960 000
// times a second. Here the mixer costs one complex multiply per sample at
// 48 kHz and the narrow filter is a handful of taps at the same rate: together a
// rounding error next to the wideband stage.
//
// Why centre at all: aviation ground stations often transmit a few kHz off the
// nominal channel (offset-carrier / "Climax" operation, where several
// transmitters cover one sector on deliberately staggered carriers), while
// aircraft transmit on the nominal frequency. A filter narrow enough to keep the
// hiss down would cut those controllers off entirely. Following the carrier
// gives both: narrow filter, nothing lost.
//
// It also cleans up Climax reception generally — with several carriers audible
// at once, keeping the strongest and filtering the rest away removes the
// heterodyne whistle between them.
type FineTuner struct {
	lpf        *RealFIRFilter // narrow low-pass, real taps applied per component
	sampleRate float64

	offsetHz float64
	phase    complex128
	step     complex128

	mixed []complex128
	out   []complex128
	// Separate real/imag scratch: the low-pass is real-valued, so each
	// component is filtered independently rather than duplicating a complex FIR.
	re, im       []float64
	reOut, imOut []float64
	lpfIm        *RealFIRFilter
}

// NewFineTuner builds a fine tuner with a low-pass of the given half-bandwidth
// (Hz from DC) operating at sampleRate.
func NewFineTuner(halfBandwidthHz, sampleRate float64, numTaps int) *FineTuner {
	taps := NewLowPassFIR(halfBandwidthHz, sampleRate, numTaps)
	return &FineTuner{
		lpf:        NewRealFIRDecimator(taps, 1),
		lpfIm:      NewRealFIRDecimator(taps, 1),
		sampleRate: sampleRate,
		phase:      complex(1, 0),
		step:       complex(1, 0),
	}
}

// SetOffset points the tuner at a carrier offsetHz from centre. The mixer phase
// is carried across the change so retuning doesn't click.
func (f *FineTuner) SetOffset(offsetHz float64) {
	if offsetHz == f.offsetHz {
		return
	}
	f.offsetHz = offsetHz
	// Mix *down* by the offset, bringing that carrier to DC.
	f.step = cmplx.Rect(1, -2*math.Pi*offsetHz/f.sampleRate)
}

// OffsetHz is the current tuning offset.
func (f *FineTuner) OffsetHz() float64 { return f.offsetHz }

// Process mixes and filters a block. The returned slice is owned by the tuner
// and valid until the next call.
func (f *FineTuner) Process(in []complex128) []complex128 {
	n := len(in)
	if cap(f.re) < n {
		f.re = make([]float64, n)
		f.im = make([]float64, n)
	}
	f.re, f.im = f.re[:n], f.im[:n]

	if f.offsetHz == 0 {
		// No shift needed; skip the mixer entirely.
		for i, s := range in {
			f.re[i], f.im[i] = real(s), imag(s)
		}
	} else {
		p := f.phase
		for i, s := range in {
			v := s * p
			f.re[i], f.im[i] = real(v), imag(v)
			p *= f.step
		}
		// Renormalise once per block rather than per sample: the magnitude
		// drifts only by rounding error over a few thousand multiplies.
		f.phase = p / complex(cmplx.Abs(p), 0)
	}

	reF := f.lpf.Process(f.re)
	imF := f.lpfIm.Process(f.im)

	m := min(len(reF), len(imF))
	if cap(f.out) < m {
		f.out = make([]complex128, m)
	}
	f.out = f.out[:m]
	for i := 0; i < m; i++ {
		f.out[i] = complex(reF[i], imF[i])
	}
	return f.out
}

// Reset clears filter history and mixer phase.
func (f *FineTuner) Reset() {
	f.lpf.Reset()
	f.lpfIm.Reset()
	f.phase = complex(1, 0)
}

// FindCarrierOffset returns the offset in Hz of the strongest spectral peak
// within ±searchHz of centre, from the output of FFT (bin 0 = DC, un-shifted).
//
// Resolution is one bin, sampleRate/len(fft). That's ample for AM: envelope
// detection is non-coherent, so a residual few hundred Hz of offset costs
// nothing — the requirement is only that the carrier and its sidebands sit
// inside the narrow filter, not that they sit exactly at DC.
func FindCarrierOffset(fft []complex128, sampleRate, searchHz float64) float64 {
	n := len(fft)
	if n == 0 {
		return 0
	}
	binHz := sampleRate / float64(n)
	maxBin := int(searchHz / binHz)
	if maxBin < 1 {
		return 0
	}
	if maxBin > n/2-1 {
		maxBin = n/2 - 1
	}

	bestPower, bestBin := -1.0, 0
	for k := -maxBin; k <= maxBin; k++ {
		idx := k
		if idx < 0 {
			idx += n // negative frequencies live in the upper half
		}
		re, im := real(fft[idx]), imag(fft[idx])
		if p := re*re + im*im; p > bestPower {
			bestPower, bestBin = p, k
		}
	}
	return float64(bestBin) * binHz
}
