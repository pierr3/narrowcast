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

// HighPassIIR is a 2nd-order (biquad) high-pass filter for removing
// low-frequency hum and carrier residuals. -12 dB/octave rolloff.
type HighPassIIR struct {
	b0, b1, b2 float64 // feedforward coefficients
	a1, a2     float64 // feedback coefficients
	x1, x2     float64 // input history
	y1, y2     float64 // output history
}

// NewHighPassIIR creates a 2nd-order Butterworth high-pass filter.
// cutoffHz is the -3dB cutoff frequency.
// sampleRate is the audio sample rate.
func NewHighPassIIR(cutoffHz float64, sampleRate float64) *HighPassIIR {
	// Bilinear transform of 2nd-order Butterworth high-pass
	w0 := 2.0 * math.Pi * cutoffHz / sampleRate
	cosW0 := math.Cos(w0)
	sinW0 := math.Sin(w0)
	alpha := sinW0 / math.Sqrt2 // Q = 0.707 (Butterworth)

	b0 := (1 + cosW0) / 2
	b1 := -(1 + cosW0)
	b2 := (1 + cosW0) / 2
	a0 := 1 + alpha
	a1 := -2 * cosW0
	a2 := 1 - alpha

	return &HighPassIIR{
		b0: b0 / a0, b1: b1 / a0, b2: b2 / a0,
		a1: a1 / a0, a2: a2 / a0,
	}
}

// Process applies high-pass filtering in-place.
func (f *HighPassIIR) Process(samples []float64) {
	for i, x := range samples {
		y := f.b0*x + f.b1*f.x1 + f.b2*f.x2 - f.a1*f.y1 - f.a2*f.y2
		f.x2 = f.x1
		f.x1 = x
		f.y2 = f.y1
		f.y1 = y
		samples[i] = y
	}
}

// SoftLimiter applies tanh-based soft clipping to prevent harsh distortion
// from ADC saturation on strong signals. Compresses loud signals smoothly
// instead of hard-clipping.
type SoftLimiter struct {
	drive float64 // controls compression knee (higher = more compression)
}

// NewSoftLimiter creates a soft limiter.
// drive controls how aggressively signals are compressed.
// 1.0 = gentle, 2.0 = moderate, 3.0+ = heavy compression.
func NewSoftLimiter(drive float64) *SoftLimiter {
	return &SoftLimiter{drive: drive}
}

// Process applies soft limiting in-place.
func (l *SoftLimiter) Process(samples []float64) {
	for i, s := range samples {
		samples[i] = math.Tanh(s * l.drive) / math.Tanh(l.drive)
	}
}

// AGC implements automatic gain control with separate attack/release smoothing.
// Uses RMS-based level detection and logarithmic gain computation.
type AGC struct {
	targetLin    float64 // target output RMS (linear)
	maxGain      float64 // maximum gain (linear)
	attackCoeff  float64 // per-sample smoothing for increasing level (fast)
	releaseCoeff float64 // per-sample smoothing for decreasing level (slow)
	envLevel     float64 // smoothed envelope level
}

// NewAGC creates an automatic gain control.
// targetDb is the desired output level in dBFS (e.g., -12).
// maxGainDb is the maximum gain in dB (e.g., 30).
// attackMs is how fast the AGC reacts to louder signals (e.g., 20ms).
// releaseMs is how fast the AGC recovers from loud signals (e.g., 500ms).
// sampleRate is the audio sample rate.
func NewAGC(targetDb float64, maxGainDb float64, attackMs float64, releaseMs float64, sampleRate float64) *AGC {
	return &AGC{
		targetLin:    math.Pow(10, targetDb/20),
		maxGain:      math.Pow(10, maxGainDb/20),
		attackCoeff:  1.0 - math.Exp(-1.0/(attackMs*0.001*sampleRate)),
		releaseCoeff: 1.0 - math.Exp(-1.0/(releaseMs*0.001*sampleRate)),
		envLevel:     0.001,
	}
}

// Process applies AGC in-place.
func (a *AGC) Process(samples []float64) {
	for i, s := range samples {
		absS := math.Abs(s)
		// Smooth envelope follower with asymmetric attack/release
		if absS > a.envLevel {
			a.envLevel += a.attackCoeff * (absS - a.envLevel)
		} else {
			a.envLevel += a.releaseCoeff * (absS - a.envLevel)
		}
		// Compute gain
		if a.envLevel > 1e-10 {
			gain := a.targetLin / a.envLevel
			if gain > a.maxGain {
				gain = a.maxGain
			}
			samples[i] = s * gain
		}
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
