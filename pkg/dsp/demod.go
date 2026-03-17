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

// NoiseGate applies a soft noise gate with attack/release smoothing.
// When signal RMS is below threshold, gain ramps down to zero.
// When above, gain ramps back up to 1.0.
type NoiseGate struct {
	thresholdLin float64 // linear amplitude threshold
	attackCoeff  float64 // per-sample coefficient for opening (fast)
	releaseCoeff float64 // per-sample coefficient for closing (slow)
	gain         float64 // current gain 0-1
}

// NewNoiseGate creates a soft noise gate.
// thresholdDb is the gate threshold in dB (e.g., -40).
// attackMs is how fast the gate opens (e.g., 5 ms).
// releaseMs is how fast the gate closes (e.g., 150 ms).
// sampleRate is the audio sample rate.
func NewNoiseGate(thresholdDb float64, attackMs float64, releaseMs float64, sampleRate float64) *NoiseGate {
	return &NoiseGate{
		thresholdLin: math.Pow(10, thresholdDb/20),
		attackCoeff:  1.0 - math.Exp(-1.0/(attackMs*0.001*sampleRate)),
		releaseCoeff: 1.0 - math.Exp(-1.0/(releaseMs*0.001*sampleRate)),
		gain:         0,
	}
}

// Process applies the noise gate in-place.
// frameSize is how many samples to measure RMS over (e.g., 160 for 10ms at 16kHz).
func (ng *NoiseGate) Process(samples []float64, frameSize int) {
	if frameSize < 1 {
		frameSize = len(samples)
	}
	for i := 0; i < len(samples); i += frameSize {
		end := i + frameSize
		if end > len(samples) {
			end = len(samples)
		}
		frame := samples[i:end]

		// Measure RMS of this frame
		var sumSq float64
		for _, s := range frame {
			sumSq += s * s
		}
		rms := math.Sqrt(sumSq / float64(len(frame)))

		// Determine target gain
		var targetGain float64
		if rms >= ng.thresholdLin {
			targetGain = 1.0
		}

		// Apply per-sample gain smoothing
		for j := range frame {
			if targetGain > ng.gain {
				ng.gain += ng.attackCoeff * (targetGain - ng.gain)
			} else {
				ng.gain += ng.releaseCoeff * (targetGain - ng.gain)
			}
			frame[j] *= ng.gain
		}
	}
}

// HighPassIIR is a single-pole high-pass filter for removing low-frequency rumble.
// Uses the same topology as DCBlocker but with a configurable cutoff.
type HighPassIIR struct {
	alpha float64
	xPrev float64
	yPrev float64
}

// NewHighPassIIR creates a single-pole high-pass filter.
// cutoffHz is the -3dB cutoff frequency.
// sampleRate is the audio sample rate.
func NewHighPassIIR(cutoffHz float64, sampleRate float64) *HighPassIIR {
	rc := 1.0 / (2.0 * math.Pi * cutoffHz)
	dt := 1.0 / sampleRate
	alpha := rc / (rc + dt)
	return &HighPassIIR{alpha: alpha}
}

// Process applies high-pass filtering in-place.
func (f *HighPassIIR) Process(samples []float64) {
	for i, x := range samples {
		y := f.alpha * (f.yPrev + x - f.xPrev)
		f.xPrev = x
		f.yPrev = y
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
