package dsp

import (
	"math"
	"math/cmplx"
)

// FMDemodulator performs FM demodulation using a polar discriminator.
type FMDemodulator struct {
	prev complex128
	gain float64
}

// NewFMDemodulator creates an FM demodulator.
// maxDeviation is the maximum frequency deviation in Hz.
// sampleRate is the input sample rate after decimation.
func NewFMDemodulator(maxDeviation float64, sampleRate float64) *FMDemodulator {
	return &FMDemodulator{
		prev: complex(1, 0),
		gain: sampleRate / (2 * math.Pi * maxDeviation),
	}
}

// Demodulate performs FM demodulation on complex IQ samples.
// Returns real-valued audio samples.
func (d *FMDemodulator) Demodulate(input []complex128) []float64 {
	output := make([]float64, len(input))
	for i, s := range input {
		// Polar discriminator: angle difference between consecutive samples
		product := s * cmplx.Conj(d.prev)
		output[i] = cmplx.Phase(product) * d.gain
		d.prev = s
	}
	return output
}

// AMDemodulator performs AM demodulation using envelope detection
// with a proper DC blocking filter.
type AMDemodulator struct {
	dcBlock *DCBlocker
}

// NewAMDemodulator creates an AM envelope detector.
func NewAMDemodulator() *AMDemodulator {
	return &AMDemodulator{
		dcBlock: NewDCBlocker(0.999),
	}
}

// Demodulate performs AM demodulation on complex IQ samples.
// Returns real-valued audio samples.
func (d *AMDemodulator) Demodulate(input []complex128) []float64 {
	output := make([]float64, len(input))
	for i, s := range input {
		output[i] = cmplx.Abs(s)
	}
	d.dcBlock.Process(output)
	return output
}

// DCBlocker is a single-pole high-pass IIR filter that removes DC offset.
// Transfer function: y[n] = x[n] - x[n-1] + alpha * y[n-1]
// alpha close to 1.0 gives a very low cutoff (~20 Hz at 16 kHz sample rate with 0.999).
type DCBlocker struct {
	alpha float64
	xPrev float64
	yPrev float64
}

// NewDCBlocker creates a DC blocking filter.
// alpha controls the cutoff: closer to 1.0 = lower cutoff frequency.
// For audio at 16 kHz, 0.999 gives ~2.5 Hz cutoff (removes DC without affecting audio).
func NewDCBlocker(alpha float64) *DCBlocker {
	return &DCBlocker{alpha: alpha}
}

// Process applies DC blocking in-place.
func (d *DCBlocker) Process(samples []float64) {
	for i, x := range samples {
		y := x - d.xPrev + d.alpha*d.yPrev
		d.xPrev = x
		d.yPrev = y
		samples[i] = y
	}
}

// DeEmphasis applies a simple single-pole de-emphasis filter.
// Used for NFM to restore the original audio frequency response.
type DeEmphasis struct {
	alpha float64
	prev  float64
}

// NewDeEmphasis creates a de-emphasis filter.
// tau is the time constant in seconds (e.g., 50e-6 for Europe, 75e-6 for US).
// sampleRate is the audio sample rate.
func NewDeEmphasis(tau float64, sampleRate float64) *DeEmphasis {
	dt := 1.0 / sampleRate
	alpha := dt / (tau + dt)
	return &DeEmphasis{alpha: alpha}
}

// Process applies de-emphasis to audio samples in-place.
func (d *DeEmphasis) Process(samples []float64) {
	for i := range samples {
		samples[i] = d.prev + d.alpha*(samples[i]-d.prev)
		d.prev = samples[i]
	}
}
