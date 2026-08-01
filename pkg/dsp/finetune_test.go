package dsp

import (
	"math"
	"math/cmplx"
	"testing"
)

// tone builds a complex exponential at freqHz.
func tone(freqHz, sampleRate float64, n int, amp float64) []complex128 {
	out := make([]complex128, n)
	for i := range out {
		out[i] = cmplx.Rect(amp, 2*math.Pi*freqHz*float64(i)/sampleRate)
	}
	return out
}

func meanMag(s []complex128) float64 {
	if len(s) == 0 {
		return 0
	}
	var sum float64
	for _, v := range s {
		sum += cmplx.Abs(v)
	}
	return sum / float64(len(s))
}

const ftRate = 48000.0

// An off-centre carrier must survive a filter far too narrow to have passed it
// where it sat. This is the offset-carrier ("Climax") case: the controller is
// several kHz off the nominal channel.
func TestFineTunerRecoversOffsetCarrier(t *testing.T) {
	ft := NewFineTuner(4000, ftRate, 65, 1)
	in := tone(7500, ftRate, 4096, 1)

	// Untuned, a ±4 kHz filter should reject a 7.5 kHz carrier hard.
	rejected := meanMag(ft.Process(in))

	ft.SetOffset(7500)
	ft.Reset()
	// Let the filter history fill before measuring.
	ft.Process(in)
	passed := meanMag(ft.Process(in))

	if passed < 0.8 {
		t.Errorf("tuned carrier attenuated: mean magnitude %.3f, want ≈1", passed)
	}
	if rejected > 0.1 {
		t.Errorf("untuned carrier leaked through: %.3f", rejected)
	}
	t.Logf("offset carrier: %.4f untuned → %.4f tuned", rejected, passed)
}

// The narrowing has to actually reject what's outside it, or there's no point.
func TestFineTunerRejectsAdjacentEnergy(t *testing.T) {
	ft := NewFineTuner(4000, ftRate, 65, 1)
	ft.SetOffset(0)

	// In-band voice sideband stays.
	ft.Process(tone(2000, ftRate, 2048, 1))
	inBand := meanMag(ft.Process(tone(2000, ftRate, 2048, 1)))

	// Energy 10 kHz out — where the hiss lives on a ±12.5 kHz channel — goes.
	ft.Reset()
	ft.Process(tone(10000, ftRate, 2048, 1))
	outBand := meanMag(ft.Process(tone(10000, ftRate, 2048, 1)))

	if inBand < 0.8 {
		t.Errorf("in-band tone attenuated to %.3f", inBand)
	}
	if outBand > 0.05 {
		t.Errorf("out-of-band tone only reduced to %.3f, want < 0.05", outBand)
	}
	t.Logf("in-band %.3f vs 10 kHz out %.4f (%.0f dB rejection)",
		inBand, outBand, 20*math.Log10(inBand/math.Max(outBand, 1e-9)))
}

func TestFineTunerZeroOffsetIsTransparentToPhase(t *testing.T) {
	ft := NewFineTuner(6000, ftRate, 33, 1)
	in := tone(1000, ftRate, 1024, 1)
	ft.Process(in)
	out := ft.Process(in)

	// With no shift, consecutive samples must keep advancing at the tone's own
	// rate — i.e. the mixer really is bypassed, not applying a stale phase.
	step := 2 * math.Pi * 1000 / ftRate
	got := cmplx.Phase(out[len(out)/2+1] * cmplx.Conj(out[len(out)/2]))
	if math.Abs(got-step) > 1e-6 {
		t.Errorf("phase step %.6f, want %.6f", got, step)
	}
}

func TestFineTunerSetOffsetKeepsPhaseContinuous(t *testing.T) {
	ft := NewFineTuner(4000, ftRate, 33, 1)
	in := tone(0, ftRate, 512, 1)
	ft.Process(in)

	// Retuning must not produce a discontinuity big enough to click. Compare the
	// last sample before the change with the first after it.
	before := ft.Process(in)
	last := before[len(before)-1]
	ft.SetOffset(1000)
	after := ft.Process(in)
	jump := cmplx.Abs(after[0] - last)
	if jump > 0.35 {
		t.Errorf("amplitude jumped %.3f across a retune — audible click", jump)
	}
}

// Decimation is folded into this stage in production, so it has to keep working
// with a decimating filter.
func TestFineTunerDecimatesWhileTuning(t *testing.T) {
	ft := NewFineTuner(3500, ftRate, 65, 3)
	ft.SetOffset(7500)
	in := tone(7500, ftRate, 3072, 1)
	ft.Process(in)
	out := ft.Process(in)

	if want := len(in) / 3; len(out) < want-2 || len(out) > want+2 {
		t.Errorf("got %d output samples, want ≈%d", len(out), want)
	}
	if m := meanMag(out); m < 0.8 {
		t.Errorf("tuned carrier attenuated to %.3f while decimating", m)
	}
}

func TestFindCarrierOffsetLocatesPeak(t *testing.T) {
	const n = 1024
	for _, want := range []float64{0, 2000, 7500, -5000, -7500} {
		buf := tone(want, ftRate, n, 1)
		FFT(buf)
		got := FindCarrierOffset(buf, ftRate, 10000)
		binHz := ftRate / n
		if math.Abs(got-want) > binHz {
			t.Errorf("carrier at %.0f Hz found at %.0f (bin %.1f Hz)", want, got, binHz)
		}
	}
}

// The search window is what stops an 8.33 kHz channel locking onto its
// neighbour 8.33 kHz away.
func TestFindCarrierOffsetRespectsSearchWindow(t *testing.T) {
	const n = 1024
	// Strong signal well outside the window, nothing inside it.
	buf := tone(9000, ftRate, n, 1)
	FFT(buf)

	if got := FindCarrierOffset(buf, ftRate, 3500); math.Abs(got) > 3500 {
		t.Errorf("found %.0f Hz outside the ±3500 Hz search window", got)
	}
	// Widen the window and it should be found.
	if got := FindCarrierOffset(buf, ftRate, 12000); math.Abs(got-9000) > ftRate/n {
		t.Errorf("with a wide window, got %.0f Hz, want 9000", got)
	}
}

func TestFindCarrierOffsetHandlesDegenerateInput(t *testing.T) {
	if got := FindCarrierOffset(nil, ftRate, 5000); got != 0 {
		t.Errorf("empty FFT gave %.0f, want 0", got)
	}
	// A search window narrower than one bin can't resolve anything.
	buf := tone(1000, ftRate, 64, 1)
	FFT(buf)
	if got := FindCarrierOffset(buf, ftRate, 100); got != 0 {
		t.Errorf("sub-bin window gave %.0f, want 0", got)
	}
}

func BenchmarkFineTuner(b *testing.B) {
	// One 20 ms AM block at the channel rate, decimating 3:1 to the audio rate
	// exactly as the AM chain does.
	ft := NewFineTuner(3500, ftRate, 65, 3)
	ft.SetOffset(7500)
	in := tone(7500, ftRate, int(ftRate*0.020), 1)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ft.Process(in)
	}
}
