package dsp

// IQDCBlocker removes the DC term a zero-IF tuner leaks into the middle of its
// own passband.
//
// Every RTL-SDR does this: the local oscillator couples into the mixer and
// appears as a constant complex offset in the IQ stream, i.e. a permanent
// spike at exactly the tuned frequency. Because narrowcast tunes the hardware
// straight to the requested frequency and runs the channel filter with zero
// offset, that spike lands precisely where the wanted carrier should be, and it
// does four separate kinds of damage:
//
//   - It is the tallest bar in the spectrum display, always, whatever is on air.
//   - Carrier tracking searches for the strongest peak near centre, so it locks
//     onto the leakage rather than an offset-carrier ground station. This is
//     invisible in simulation, where there is no leakage to lock onto.
//   - Channel power reads high, which biases both the S-meter and the squelch.
//   - AM is envelope-detected, so a steady spurious carrier reduces the apparent
//     modulation depth — and the AGC then divides by the inflated envelope,
//     making genuinely received audio quieter.
//
// The estimate is only refined while no signal is present, which is the whole
// trick: an AM carrier tuned dead on frequency is *also* at DC, and a blocker
// that ran continuously would subtract the very thing it is meant to receive.
// Leakage is steady over minutes while transmissions last seconds, so freezing
// the estimate for the duration of a transmission costs nothing.
type IQDCBlocker struct {
	dc      complex128
	alpha   float64
	refines int
}

// primeBlocks is how many blocks the estimate is refined unconditionally after
// a reset, before it defers to the caller's signal-present test.
//
// Without this the thing never starts: leakage is what inflates channel power,
// so an uncorrected receiver reads a dead channel as busy, holds the squelch
// open, and therefore never offers a quiet block to learn from. Priming through
// a transmission is self-healing — the estimate absorbs some carrier, then the
// first genuinely quiet block afterwards pulls it back.
const primeBlocks = 100

// defaultDCAlpha adapts with a time constant around 20 blocks — roughly 400 ms
// at 20 ms per block. Slow enough that the noise in any one block barely moves
// the estimate, fast enough to converge shortly after the receiver starts.
const defaultDCAlpha = 0.05

// NewIQDCBlocker creates a DC blocker. alpha <= 0 selects the default.
func NewIQDCBlocker(alpha float64) *IQDCBlocker {
	if alpha <= 0 {
		alpha = defaultDCAlpha
	}
	return &IQDCBlocker{alpha: alpha}
}

// Process subtracts the current estimate in place.
func (d *IQDCBlocker) Process(iq []complex128) {
	if d.dc == 0 {
		return
	}
	dc := d.dc
	for i := range iq {
		iq[i] -= dc
	}
}

// Refine nudges the estimate using the mean of a block that has already been
// through Process — so this measures what is left over, and converges on the
// true offset rather than assuming one block's mean is it.
//
// Call this only for blocks with no signal present. See the type comment.
func (d *IQDCBlocker) Refine(residual []complex128) {
	if len(residual) == 0 {
		return
	}
	var sumRe, sumIm float64
	for _, s := range residual {
		sumRe += real(s)
		sumIm += imag(s)
	}
	n := float64(len(residual))
	mean := complex(sumRe/n, sumIm/n)
	d.dc += complex(d.alpha, 0) * mean
	if d.refines < primeBlocks {
		d.refines++
	}
}

// Priming reports that the estimate is still converging and should be refined
// even on blocks where a signal appears to be present. See primeBlocks.
func (d *IQDCBlocker) Priming() bool { return d.refines < primeBlocks }

// Offset is the current estimate. Diagnostics only.
func (d *IQDCBlocker) Offset() complex128 { return d.dc }

// Reset discards the estimate and re-arms priming. The offset is a property of
// the tuner at its current frequency, so a retune invalidates it.
func (d *IQDCBlocker) Reset() {
	d.dc = 0
	d.refines = 0
}
