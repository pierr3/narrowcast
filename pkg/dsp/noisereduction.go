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
// The noise estimate is a per-bin minimum tracked over ~1.6 s while the gate is
// open, which is longer than any syllable, so what it measures is the floor
// underneath the speech rather than the speech.
//
// The obvious alternative — learn during a closed squelch, as IQDCBlocker does
// — was tried first and is wrong here. A closed squelch means no carrier, and
// envelope detection gives noise a different magnitude and shape depending on
// whether there is a carrier for it to ride on. An estimate taken from dead air
// therefore mis-describes the noise present during a transmission, which is the
// only time it is ever used. On real air that mistake cost about 10 dB of the
// available reduction. Closed-squelch frames are still used, but only to seed
// something usable before the first minimum window has filled.
//
// This is the one stage in the chain that can make things sound worse rather
// than merely not better, which is why NROff is a true bypass.
type SpectralNR struct {
	level    NRLevel
	set      nrSettings
	fftSize  int
	hop      int
	window   []float64
	noiseMag []float64 // per-bin noise magnitude estimate in use
	gain     []float64 // per-bin gain, smoothed over time
	learned  int       // squelch-closed frames seen (bootstrap only)

	// Minimum statistics, tracked while the gate is OPEN. See noteMinimum.
	smoothPow []float64   // variance-reduced power per bin
	subMin    [][]float64 // ring of per-sub-window minima
	curMin    []float64   // minimum accumulating in the current sub-window
	subIdx    int         // which ring slot the current sub-window fills
	subCount  int         // frames into the current sub-window
	subFilled int         // ring slots populated so far

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

	// Minimum statistics. The per-bin minimum of a smoothed power spectrum over
	// a window longer than any syllable is, by construction, a measurement of
	// the noise floor and not of the speech on top of it.
	//
	// This is what the noise estimate is really built from, because the obvious
	// alternative does not work for AM. Learning during a closed squelch means
	// learning with no carrier present, and envelope detection gives noise a
	// different magnitude and shape depending on whether a carrier is there to
	// ride on. An estimate taken from dead air therefore mis-describes the noise
	// during a transmission, which is the only time it gets used.
	nrSubWindowFrames = 48 // ~0.4 s at 16 kHz with a 128-sample hop
	nrSubWindows      = 4  // so the minimum spans ~1.6 s
	// nrPowSmoothing averages the periodogram before minima are taken; without
	// it the minimum of a noisy estimate sits well below the true floor.
	nrPowSmoothing = 0.7
	// nrMinBias compensates for what is left of that downward bias. The minimum
	// of even a smoothed spectrum still underestimates its mean.
	nrMinBias = 1.5
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
	bins := n/2 + 1
	nr := &SpectralNR{
		level:     level,
		set:       level.settings(),
		fftSize:   n,
		hop:       n / 2,
		window:    w,
		noiseMag:  make([]float64, bins),
		gain:      make([]float64, bins),
		frame:     make([]complex128, n),
		smoothPow: make([]float64, bins),
		curMin:    make([]float64, bins),
	}
	nr.subMin = make([][]float64, nrSubWindows)
	for i := range nr.subMin {
		nr.subMin[i] = make([]float64, bins)
	}
	nr.resetMinima()
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

// Ready reports whether there is an estimate worth subtracting yet.
func (s *SpectralNR) Ready() bool {
	return s.subFilled > 0 || s.learned >= nrMinLearnFrames
}

// resetMinima clears the minimum tracker.
func (s *SpectralNR) resetMinima() {
	for i := range s.curMin {
		s.curMin[i] = math.Inf(1)
	}
	for _, w := range s.subMin {
		for i := range w {
			w[i] = math.Inf(1)
		}
	}
	s.subIdx, s.subCount, s.subFilled = 0, 0, 0
	for i := range s.smoothPow {
		s.smoothPow[i] = 0
	}
}

// noteMinimum folds one frame's spectrum into the minimum tracker and refreshes
// the noise estimate from it.
//
// Smoothing the periodogram first matters: the minimum of a raw, noisy estimate
// sits far below the true floor, and the whole method depends on the minimum
// being a fair measurement of it.
func (s *SpectralNR) noteMinimum(half int) {
	for k := 0; k <= half; k++ {
		re, im := real(s.frame[k]), imag(s.frame[k])
		p := re*re + im*im
		if s.smoothPow[k] == 0 {
			s.smoothPow[k] = p
		} else {
			s.smoothPow[k] = nrPowSmoothing*s.smoothPow[k] + (1-nrPowSmoothing)*p
		}
		if s.smoothPow[k] < s.curMin[k] {
			s.curMin[k] = s.smoothPow[k]
		}
	}

	s.subCount++
	if s.subCount < nrSubWindowFrames {
		return
	}
	// Close this sub-window and rotate.
	copy(s.subMin[s.subIdx], s.curMin)
	s.subIdx = (s.subIdx + 1) % nrSubWindows
	s.subCount = 0
	if s.subFilled < nrSubWindows {
		s.subFilled++
	}
	for i := range s.curMin {
		s.curMin[i] = math.Inf(1)
	}

	// Estimate is the smallest sub-window minimum, bias-corrected.
	for k := 0; k <= half; k++ {
		best := math.Inf(1)
		for w := 0; w < s.subFilled; w++ {
			if v := s.subMin[w][k]; v < best {
				best = v
			}
		}
		if math.IsInf(best, 1) {
			continue
		}
		s.noiseMag[k] = nrMinBias * math.Sqrt(best)
	}
}

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
		// Bootstrap only, and only until the minimum tracker has something. See
		// the type comment for why dead air is the wrong teacher for AM.
		if s.subFilled == 0 {
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
		}
	} else {
		s.noteMinimum(half)
	}
	if !learnNoise && s.Ready() {
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
	s.resetMinima()
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
