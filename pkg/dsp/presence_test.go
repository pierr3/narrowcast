package dsp

import (
	"math"
	"testing"
)

const presRate = 16000.0

// gainAt measures the filter's steady-state gain at freqHz by driving it with a
// tone and comparing output to input amplitude, discarding the transient.
func gainAt(f *PresenceEQ, freqHz float64) float64 {
	const n = 8192
	in := make([]float64, n)
	for i := range in {
		in[i] = math.Sin(2 * math.Pi * freqHz * float64(i) / presRate)
	}
	out := append([]float64(nil), in...)
	f.Reset()
	f.Process(out)

	// Second half only: the first is the filter settling.
	var peak float64
	for _, v := range out[n/2:] {
		if a := math.Abs(v); a > peak {
			peak = a
		}
	}
	return 20 * math.Log10(peak)
}

// The point of the filter: consonant energy up, everything else where it was.
func TestPresenceEQLiftsTheConsonantBand(t *testing.T) {
	f := NewPresenceEQ(2000, 0.9, 5, presRate)

	for _, c := range []struct {
		hz     float64
		lo, hi float64
		what   string
	}{
		{300, -1.0, 1.0, "low voice, must be left alone"},
		{700, -0.5, 2.5, "vowel body, barely touched"},
		{2000, 4.0, 5.5, "centre of the lift"},
		{2800, 1.5, 5.0, "upper consonants, still lifted"},
	} {
		g := gainAt(f, c.hz)
		if g < c.lo || g > c.hi {
			t.Errorf("%.0f Hz (%s): %.2f dB, want %.1f..%.1f", c.hz, c.what, g, c.lo, c.hi)
		}
		t.Logf("%5.0f Hz → %+5.2f dB  (%s)", c.hz, g, c.what)
	}
}

// Boost must be bounded by what was asked for, or gain staging downstream is
// guesswork.
func TestPresenceEQRespectsRequestedGain(t *testing.T) {
	for _, db := range []float64{3, 5, 8} {
		f := NewPresenceEQ(2000, 0.9, db, presRate)
		g := gainAt(f, 2000)
		if math.Abs(g-db) > 0.6 {
			t.Errorf("asked for %.0f dB, measured %.2f dB at centre", db, g)
		}
	}
}

// Zero gain has to be a true bypass, so disabling it costs nothing but cycles.
func TestPresenceEQZeroGainIsTransparent(t *testing.T) {
	f := NewPresenceEQ(2000, 0.9, 0, presRate)
	for _, hz := range []float64{300, 1000, 2000, 3000} {
		if g := gainAt(f, hz); math.Abs(g) > 0.05 {
			t.Errorf("%.0f Hz: %.3f dB, want 0", hz, g)
		}
	}
}

// A biquad with bad coefficients rings or blows up; make sure a long run stays
// bounded and settles.
func TestPresenceEQIsStable(t *testing.T) {
	f := NewPresenceEQ(2000, 0.9, 6, presRate)
	buf := make([]float64, 4096)
	buf[0] = 1 // impulse
	f.Process(buf)
	for i, v := range buf {
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 10 {
			t.Fatalf("impulse response diverged at sample %d: %v", i, v)
		}
	}
	// Tail must have decayed away.
	var tail float64
	for _, v := range buf[2048:] {
		tail += math.Abs(v)
	}
	if tail > 1e-6 {
		t.Errorf("impulse response still ringing: tail sum %.3g", tail)
	}
}

func TestPresenceEQResetClearsHistory(t *testing.T) {
	f := NewPresenceEQ(2000, 0.9, 6, presRate)
	loud := make([]float64, 256)
	for i := range loud {
		loud[i] = 1
	}
	f.Process(loud)
	f.Reset()

	quiet := make([]float64, 8)
	f.Process(quiet)
	for i, v := range quiet {
		if v != 0 {
			t.Errorf("sample %d = %v after Reset, want 0", i, v)
		}
	}
}

func BenchmarkPresenceEQ(b *testing.B) {
	f := NewPresenceEQ(2000, 0.9, 5, presRate)
	// One 20 ms block at the AM audio rate.
	buf := make([]float64, 320)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		f.Process(buf)
	}
}
