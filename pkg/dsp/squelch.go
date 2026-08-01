package dsp

import "math"

// Squelch decides whether demodulated audio should be passed through.
//
// It gates on **channel power** — the level of the filtered RF channel, before
// demodulation — not on how loud the recovered audio is. That distinction is the
// whole point:
//
//   - An AM carrier is present at constant amplitude for the entire
//     transmission, whether the speaker is mid-word or mid-pause.
//   - FM is constant-envelope, so its channel power doesn't move with
//     modulation either.
//   - Audio level, by contrast, dips between syllables. Gating on it makes the
//     squelch chatter in the middle of speech, which is exactly what listeners
//     experience as a "pointy", fiddly threshold that can never be set right.
//
// Two more mechanisms stop a threshold sitting on the edge of a signal from
// flapping, both standard in receivers and neither previously present here:
//
//   - Hysteresis: the gate opens at the threshold but only closes some dB
//     below it, so noise wobbling around the set point can't toggle it.
//   - Hang time: once open it stays open briefly after the signal goes, which
//     bridges syllable gaps, short fades, and the momentary drop as an aircraft
//     transmitter unkeys.
type Squelch struct {
	thresholdDb  float64
	hysteresisDb float64
	hangSamples  int

	// attack/decay smoothing on the measured level, so one noisy block can't
	// swing the decision on its own.
	attack float64
	decay  float64

	levelDb  float64
	seeded   bool
	open     bool
	hangLeft int
}

// NewSquelch builds a squelch gate.
//
//	thresholdDb  — channel power at which the gate opens (dBFS, negative)
//	hysteresisDb — how far below the threshold it must fall to close again
//	hangMs       — how long to stay open after the signal drops
//	sampleRate   — rate of the samples passed to Update (the channel rate)
func NewSquelch(thresholdDb, hysteresisDb, hangMs, sampleRate float64) *Squelch {
	if hysteresisDb < 0 {
		hysteresisDb = -hysteresisDb
	}
	return &Squelch{
		thresholdDb:  thresholdDb,
		hysteresisDb: hysteresisDb,
		hangSamples:  int(hangMs / 1000 * sampleRate),
		// Rise quickly so the start of a transmission isn't clipped; fall
		// slowly so a brief dip doesn't reach the close threshold at all.
		attack: 0.5,
		decay:  0.05,
	}
}

// ChannelPowerDb returns the mean power of an IQ block in dBFS, the measurement
// Update expects. Full-scale IQ is 0 dB.
func ChannelPowerDb(iq []complex128) float64 {
	if len(iq) == 0 {
		return -120
	}
	var sum float64
	for _, s := range iq {
		re, im := real(s), imag(s)
		sum += re*re + im*im
	}
	mean := sum / float64(len(iq))
	if mean < 1e-20 {
		return -200
	}
	return 10 * math.Log10(mean)
}

// Update feeds one block's channel power plus the number of samples it covered,
// and reports whether audio should be passed for this block.
func (s *Squelch) Update(powerDb float64, samples int) bool {
	if !s.seeded {
		s.levelDb = powerDb
		s.seeded = true
	} else {
		k := s.decay
		if powerDb > s.levelDb {
			k = s.attack
		}
		s.levelDb += (powerDb - s.levelDb) * k
	}

	switch {
	case s.levelDb >= s.thresholdDb:
		s.open = true
		s.hangLeft = s.hangSamples
	case s.open && s.levelDb >= s.thresholdDb-s.hysteresisDb:
		// In the hysteresis band: hold state, don't spend hang time. This is
		// where a speech dip lands, and holding here is what stops the chatter.
	case s.open:
		// Below the close threshold — run the hang timer down before closing.
		s.hangLeft -= samples
		if s.hangLeft <= 0 {
			s.open = false
			s.hangLeft = 0
		}
	}
	return s.open
}

// SetThreshold retunes the open threshold. Safe to call every block; only an
// actual change has any effect.
//
// A change re-evaluates the gate immediately rather than granting it the
// hysteresis band. Hysteresis exists to absorb *the signal* wobbling around a
// fixed set point — it should not mean that dragging the slider above a signal
// leaves it audible, which is what a listener reads as a broken control.
func (s *Squelch) SetThreshold(db float64) {
	if db == s.thresholdDb {
		return
	}
	s.thresholdDb = db
	if s.seeded && s.levelDb < db {
		s.open = false
		s.hangLeft = 0
	}
}

// LevelDb is the smoothed channel power, for display. Reporting the same
// quantity the gate uses is what lets a user aim the threshold at what they see.
func (s *Squelch) LevelDb() float64 { return s.levelDb }

// IsOpen reports the current gate state.
func (s *Squelch) IsOpen() bool { return s.open }

// Reset clears level and gate state. Call on retune or after an IQ gap, where
// the previous level says nothing about the new signal.
func (s *Squelch) Reset() {
	s.seeded = false
	s.open = false
	s.hangLeft = 0
}
