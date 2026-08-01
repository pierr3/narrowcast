package dsp

import (
	"math"
	"math/cmplx"
	"math/rand"
	"testing"
)

// noisyBlock builds a block of complex noise sitting on a constant DC offset,
// which is what a zero-IF tuner delivers on a dead channel.
func noisyBlock(n int, dc complex128, sigma float64, rng *rand.Rand) []complex128 {
	out := make([]complex128, n)
	for i := range out {
		out[i] = dc + complex(rng.NormFloat64()*sigma, rng.NormFloat64()*sigma)
	}
	return out
}

func TestIQDCBlockerConvergesOnLeakage(t *testing.T) {
	const want = complex(0.35, -0.2)
	rng := rand.New(rand.NewSource(1))
	b := NewIQDCBlocker(0)

	// 100 blocks of dead air, as at startup.
	for i := 0; i < 100; i++ {
		blk := noisyBlock(1024, want, 0.05, rng)
		b.Process(blk)
		b.Refine(blk)
	}

	if err := cmplx.Abs(b.Offset() - want); err > 0.01 {
		t.Errorf("estimate %v, want %v (error %.4f)", b.Offset(), want, err)
	}

	// A fresh block should come out centred on zero.
	blk := noisyBlock(4096, want, 0.05, rng)
	b.Process(blk)
	var sum complex128
	for _, s := range blk {
		sum += s
	}
	if residual := cmplx.Abs(sum / complex(float64(len(blk)), 0)); residual > 0.01 {
		t.Errorf("residual DC %.4f after correction, want ~0", residual)
	}
}

// The reason Refine is gated on the squelch: a carrier tuned dead on frequency
// sits at DC too, and a blocker that kept adapting through a transmission would
// eat it.
func TestIQDCBlockerDoesNotEatAnOnFrequencyCarrier(t *testing.T) {
	const leakage = complex(0.3, 0.1)
	rng := rand.New(rand.NewSource(2))
	b := NewIQDCBlocker(0)

	// Converge on dead air first.
	for i := 0; i < 100; i++ {
		blk := noisyBlock(1024, leakage, 0.05, rng)
		b.Process(blk)
		b.Refine(blk)
	}
	settled := b.Offset()

	// Now a strong on-frequency carrier arrives: DC plus a real signal at 0 Hz.
	// Refine is NOT called, because the squelch is open.
	const carrier = 0.8
	for i := 0; i < 100; i++ {
		blk := noisyBlock(1024, leakage+complex(carrier, 0), 0.05, rng)
		b.Process(blk)
	}
	if drift := cmplx.Abs(b.Offset() - settled); drift > 1e-9 {
		t.Errorf("estimate drifted by %.4f during a transmission", drift)
	}

	// And the carrier survives, minus only the leakage.
	blk := noisyBlock(4096, leakage+complex(carrier, 0), 0.05, rng)
	b.Process(blk)
	var sum complex128
	for _, s := range blk {
		sum += s
	}
	got := real(sum / complex(float64(len(blk)), 0))
	if math.Abs(got-carrier) > 0.02 {
		t.Errorf("carrier came out at %.3f, want %.3f", got, carrier)
	}
}

// Blocking must not attenuate a signal that is off DC, which is everything the
// receiver actually wants.
func TestIQDCBlockerLeavesOffsetSignalAlone(t *testing.T) {
	b := NewIQDCBlocker(0)
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 100; i++ {
		blk := noisyBlock(1024, complex(0.4, 0.4), 0.05, rng)
		b.Process(blk)
		b.Refine(blk)
	}

	// A tone 5 kHz off centre, on top of the leakage.
	const n = 4096
	sig := make([]complex128, n)
	for i := range sig {
		sig[i] = complex(0.4, 0.4) + cmplx.Rect(1, 2*math.Pi*5000*float64(i)/48000)
	}
	before := meanMag(sig)
	b.Process(sig)
	after := meanMag(sig)

	// Before correction the leakage inflates the magnitude; after, the tone
	// should stand alone at amplitude 1.
	if math.Abs(after-1.0) > 0.05 {
		t.Errorf("off-DC tone came out at %.3f, want 1.0 (was %.3f with leakage)", after, before)
	}
}

func TestIQDCBlockerResetClearsEstimate(t *testing.T) {
	b := NewIQDCBlocker(0)
	rng := rand.New(rand.NewSource(4))
	blk := noisyBlock(1024, complex(0.5, 0.5), 0.01, rng)
	b.Process(blk)
	b.Refine(blk)
	if b.Offset() == 0 {
		t.Fatal("estimate did not move")
	}
	b.Reset()
	if b.Offset() != 0 {
		t.Errorf("offset %v after Reset, want 0", b.Offset())
	}
}

func BenchmarkIQDCBlocker(b *testing.B) {
	blk := make([]complex128, 19200) // one 20 ms block at 960 kS/s
	for i := range blk {
		blk[i] = complex(0.3, 0.2)
	}
	d := NewIQDCBlocker(0)
	d.Refine(blk)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		d.Process(blk)
	}
}
