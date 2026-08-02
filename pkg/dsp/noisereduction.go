package dsp

import "math"

// NRLevel selects how hard the noise reduction works.
type NRLevel int

const (
	NROff NRLevel = iota
	NRLight
	NRMedium
	NRStrong
)

// nrSettings holds the two numbers that define a level.
//
//	overSubtract — how much more than the estimated noise to remove. Above 1 it
//	    compensates for the estimate being an average while any individual frame
//	    fluctuates around it; too high and it starts eating the quiet parts of
//	    speech.
//	floorGain — the least a bin may be attenuated to. This is the setting that
//	    decides whether the result sounds clean or awful. Subtracting all the way
//	    to zero leaves isolated surviving bins scattered across time and
//	    frequency, which the ear hears as "musical noise": a warbling of random
//	    tones that is more objectionable than the steady hiss it replaced. A
//	    floor keeps a little of the original noise, and that residue is what
//	    masks the warble.
type nrSettings struct {
	overSubtract float64
	floorGain    float64
}

func (l NRLevel) settings() nrSettings {
	switch l {
	case NRLight:
		return nrSettings{overSubtract: 1.0, floorGain: 0.25} // -12 dB floor
	case NRMedium:
		return nrSettings{overSubtract: 1.5, floorGain: 0.15} // -16 dB
	case NRStrong:
		return nrSettings{overSubtract: 2.0, floorGain: 0.08} // -22 dB
	default:
		return nrSettings{overSubtract: 0, floorGain: 1}
	}
}

// SpectralNR removes stationary broadband noise from demodulated voice by
// subtracting an estimate of the noise spectrum from each short-time frame.
//
// Measured on real airband traffic, the hiss is flat across 400-2500 Hz and
// sits about 20 dB under the speech — inside the voice passband, so no filter
// can reach it without taking the voice too. Subtracting it per frequency bin
// can, because speech occupies any given bin only some of the time while the
// noise is always there.
//
// The noise estimate comes from blocks where the squelch is shut, which is the
// same trick IQDCBlocker and the client's squelch calibrator use: the gate has
// already decided when there is no signal, so there is no need to infer it
// again from the audio. The estimate freezes while the gate is open, so speech
// can never be learned as noise.
//
// This is the one stage in the chain that can make things sound worse rather
// than merely not better, which is why NROff is a true bypass.
type SpectralNR struct {
	level    NRLevel
	set      nrSettings
	fftSize  int
	hop      int
	window   []float64
	noiseMag []float64 // per-bin noise magnitude estimate
	gain     []float64 // per-bin gain, smoothed over time
	learned  int

	// Input waiting to be framed, and output accumulated by overlap-add.
	inBuf  []float64
	outBuf []float64
	ready  int // samples of outBuf that are complete and may be emitted

	frame []complex128
	out   []float64
}

const (
	// nrFFTSize at 16 kHz is a 16 ms window with 62.5 Hz bins. Longer windows
	// resolve frequency better, but the algorithmic delay is one window, and
	// after a week spent removing latency from this stream 16 ms is a better
	// trade than 32.
	nrFFTSize = 256
	// nrGainSmoothing damps how fast a bin's gain may change between frames.
	// Instantaneous gains fluctuate with the noise itself, and that flutter is
	// what musical noise is made of.
	nrGainSmoothing = 0.5
	// nrNoiseAlpha adapts the noise estimate over roughly 30 frames of dead air.
	nrNoiseAlpha = 0.05
	// nrMinLearnFrames is how many noise frames must be seen before any
	// subtraction happens. Subtracting a half-formed estimate is worse than
	// leaving the audio alone.
	nrMinLearnFrames = 20
)

// NewSpectralNR builds a noise reducer for the given audio rate.
func NewSpectralNR(level NRLevel) *SpectralNR {
	n := nrFFTSize
	w := make([]float64, n)
	for i := range w {
		// Periodic Hann. At 50% overlap it sums to exactly 1, so overlap-add
		// needs no synthesis window and no normalisation.
		w[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n))
	}
	nr := &SpectralNR{
		level:    level,
		set:      level.settings(),
		fftSize:  n,
		hop:      n / 2,
		window:   w,
		noiseMag: make([]float64, n/2+1),
		gain:     make([]float64, n/2+1),
		frame:    make([]complex128, n),
	}
	for i := range nr.gain {
		nr.gain[i] = 1
	}
	return nr
}

// SetLevel changes strength at runtime. Off is a bypass.
func (s *SpectralNR) SetLevel(l NRLevel) {
	s.level = l
	s.set = l.settings()
}

// Level reports the current strength.
func (s *SpectralNR) Level() NRLevel { return s.level }

// Ready reports whether enough noise-only audio has been seen to subtract
// anything yet.
func (s *SpectralNR) Ready() bool { return s.learned >= nrMinLearnFrames }

// Process runs one block. When learnNoise is true the block is taken to be
// noise only — the caller guarantees this by only setting it while the squelch
// is shut — and it refines the estimate instead of being denoised.
//
// The returned slice is owned by the reducer and valid until the next call, in
// keeping with the rest of the package. Output lags input by one window while
// the first frame fills.
func (s *SpectralNR) Process(in []float64, learnNoise bool) []float64 {
	if s.level == NROff {
		return in
	}
	s.inBuf = append(s.inBuf, in...)

	for len(s.inBuf) >= s.fftSize {
		s.analyse(learnNoise)
		s.inBuf = s.inBuf[:copy(s.inBuf, s.inBuf[s.hop:])]
	}

	// Emit whatever overlap-add has completed.
	n := s.ready
	if cap(s.out) < n {
		s.out = make([]float64, n)
	}
	s.out = s.out[:n]
	copy(s.out, s.outBuf[:n])
	s.outBuf = s.outBuf[:copy(s.outBuf, s.outBuf[n:])]
	s.ready = 0
	return s.out
}

// analyse transforms one window, applies the per-bin gain, and overlap-adds the
// result back.
func (s *SpectralNR) analyse(learnNoise bool) {
	half := s.fftSize / 2
	for i := 0; i < s.fftSize; i++ {
		s.frame[i] = complex(s.inBuf[i]*s.window[i], 0)
	}
	FFT(s.frame)

	if learnNoise {
		for k := 0; k <= half; k++ {
			mag := cabs(s.frame[k])
			if s.learned == 0 {
				s.noiseMag[k] = mag
			} else {
				s.noiseMag[k] += nrNoiseAlpha * (mag - s.noiseMag[k])
			}
		}
		if s.learned < nrMinLearnFrames {
			s.learned++
		}
	} else if s.Ready() {
		for k := 0; k <= half; k++ {
			mag := cabs(s.frame[k])
			target := s.set.floorGain
			if mag > 1e-12 {
				// Magnitude subtraction, clamped so a bin is attenuated but
				// never removed outright.
				g := (mag - s.set.overSubtract*s.noiseMag[k]) / mag
				if g > target {
					target = g
				}
				if target > 1 {
					target = 1
				}
			}
			s.gain[k] += nrGainSmoothing * (target - s.gain[k])
		}
		for k := 0; k <= half; k++ {
			g := complex(s.gain[k], 0)
			s.frame[k] *= g
			// Keep the spectrum conjugate-symmetric so the inverse transform is
			// real; bin 0 and Nyquist have no mirror.
			if k > 0 && k < half {
				s.frame[s.fftSize-k] *= g
			}
		}
	}

	ifft(s.frame)

	// Overlap-add. Hann at 50% overlap sums to 1, so no normalisation.
	need := len(s.outBuf) + s.hop
	for len(s.outBuf) < need {
		s.outBuf = append(s.outBuf, 0)
	}
	base := len(s.outBuf) - s.fftSize
	if base < 0 {
		// First window: grow so the whole thing fits.
		for len(s.outBuf) < s.fftSize {
			s.outBuf = append(s.outBuf, 0)
		}
		base = 0
	}
	for i := 0; i < s.fftSize; i++ {
		s.outBuf[base+i] += real(s.frame[i])
	}
	s.ready += s.hop
}

// Reset clears buffers and the learned noise estimate. The estimate belongs to
// the current channel, so a retune invalidates it.
func (s *SpectralNR) Reset() {
	s.inBuf = s.inBuf[:0]
	s.outBuf = s.outBuf[:0]
	s.ready = 0
	s.learned = 0
	for i := range s.noiseMag {
		s.noiseMag[i] = 0
	}
	for i := range s.gain {
		s.gain[i] = 1
	}
}

func cabs(c complex128) float64 {
	re, im := real(c), imag(c)
	return math.Sqrt(re*re + im*im)
}

// ifft is the inverse transform, via the conjugate identity so the forward FFT
// is the only transform implemented.
func ifft(a []complex128) {
	for i, v := range a {
		a[i] = complex(real(v), -imag(v))
	}
	FFT(a)
	n := float64(len(a))
	for i, v := range a {
		a[i] = complex(real(v)/n, -imag(v)/n)
	}
}
