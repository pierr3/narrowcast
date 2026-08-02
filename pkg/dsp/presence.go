package dsp

import "math"

// PresenceEQ is a peaking filter that lifts the consonant band of AM voice.
//
// Airband AM sounds muffled, and the reason is not the receiver: the
// transmitted audio is band-limited, envelope detection adds nothing above the
// modulating frequencies, and the voice low-pass then closes the top at 3 kHz.
// What suffers is intelligibility rather than level — t, k, s and f are
// distinguished almost entirely by energy between roughly 1.5 and 3 kHz, so a
// broad lift there is most of what makes a communications receiver sound crisp
// instead of woolly. The practical benefit is copying a callsign or a squawk
// first time.
//
// This does not improve signal-to-noise ratio and is not meant to: it lifts
// noise in that band by exactly as much as it lifts voice. It changes how
// audible the consonants are, not how much noise sits behind them.
//
// A peaking filter rather than a high shelf, because a shelf would keep rising
// into the region the voice low-pass is already removing, spending its boost
// where there is nothing left to boost.
type PresenceEQ struct {
	b0, b1, b2 float64
	a1, a2     float64
	x1, x2     float64
	y1, y2     float64
}

// NewPresenceEQ builds a peaking EQ from the RBJ audio-EQ cookbook.
//
//	freqHz   — centre of the lift
//	q        — width; lower is broader. Around 0.9 covers ~1.2-3.3 kHz for a
//	           2 kHz centre, which matches the band the voice filter passes.
//	gainDb   — peak boost. Positive lifts.
func NewPresenceEQ(freqHz, q, gainDb, sampleRate float64) *PresenceEQ {
	a := math.Pow(10, gainDb/40)
	w0 := 2 * math.Pi * freqHz / sampleRate
	cosW0 := math.Cos(w0)
	alpha := math.Sin(w0) / (2 * q)

	b0 := 1 + alpha*a
	b1 := -2 * cosW0
	b2 := 1 - alpha*a
	a0 := 1 + alpha/a
	a1 := -2 * cosW0
	a2 := 1 - alpha/a

	return &PresenceEQ{
		b0: b0 / a0, b1: b1 / a0, b2: b2 / a0,
		a1: a1 / a0, a2: a2 / a0,
	}
}

// Process applies the filter in place.
func (f *PresenceEQ) Process(samples []float64) {
	for i, x := range samples {
		y := f.b0*x + f.b1*f.x1 + f.b2*f.x2 - f.a1*f.y1 - f.a2*f.y2
		f.x2, f.x1 = f.x1, x
		f.y2, f.y1 = f.y1, y
		samples[i] = y
	}
}

// Reset clears filter history.
func (f *PresenceEQ) Reset() {
	f.x1, f.x2, f.y1, f.y2 = 0, 0, 0, 0
}
