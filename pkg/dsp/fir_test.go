package dsp

import (
	"math"
	"math/cmplx"
	"testing"
)

// naiveRealFIR is the textbook definition the optimized filters must match:
// output n = Σ x[n-(ntaps-1)+j]·taps[j], zero-padded before the start of the
// signal, keeping every decim'th sample.
func naiveRealFIR(in, taps []float64, decim int) []float64 {
	ntaps := len(taps)
	var out []float64
	for i := 0; i < len(in); i++ {
		if (i+1)%decim != 0 {
			continue
		}
		var acc float64
		for j := 0; j < ntaps; j++ {
			idx := i - (ntaps - 1) + j
			if idx < 0 {
				continue
			}
			acc += in[idx] * taps[j]
		}
		out = append(out, acc)
	}
	return out
}

func naiveComplexFIR(in []complex128, taps []float64, decim int) []complex128 {
	ntaps := len(taps)
	var out []complex128
	for i := 0; i < len(in); i++ {
		if (i+1)%decim != 0 {
			continue
		}
		var acc complex128
		for j := 0; j < ntaps; j++ {
			idx := i - (ntaps - 1) + j
			if idx < 0 {
				continue
			}
			acc += in[idx] * complex(taps[j], 0)
		}
		out = append(out, acc)
	}
	return out
}

func ramp(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = math.Sin(float64(i)*0.1) + 0.25*math.Sin(float64(i)*1.7)
	}
	return out
}

func complexRamp(n int) []complex128 {
	out := make([]complex128, n)
	for i := range out {
		out[i] = complex(math.Sin(float64(i)*0.05), math.Cos(float64(i)*0.11))
	}
	return out
}

const tol = 1e-12

func TestRealFIRMatchesDirectConvolution(t *testing.T) {
	taps := NewLowPassFIR(2000, 16000, 31)
	in := ramp(512)

	for _, decim := range []int{1, 2, 4, 8} {
		f := NewRealFIRDecimator(taps, decim)
		got := append([]float64(nil), f.Process(in)...)
		want := naiveRealFIR(in, taps, decim)

		if len(got) != len(want) {
			t.Fatalf("decim %d: got %d outputs, want %d", decim, len(got), len(want))
		}
		for i := range want {
			if math.Abs(got[i]-want[i]) > tol {
				t.Fatalf("decim %d: output %d = %g, want %g", decim, i, got[i], want[i])
			}
		}
	}
}

// Block boundaries are where a filter with history state gets it wrong. Feeding
// the same signal in chunks must produce exactly the single-block result.
func TestRealFIRIsBlockSizeInvariant(t *testing.T) {
	taps := NewLowPassFIR(3000, 16000, 65)
	in := ramp(1200)
	decim := 3

	whole := append([]float64(nil), NewRealFIRDecimator(taps, decim).Process(in)...)

	for _, chunk := range []int{1, 7, 60, 300, 999} {
		f := NewRealFIRDecimator(taps, decim)
		var got []float64
		for start := 0; start < len(in); start += chunk {
			end := min(start+chunk, len(in))
			got = append(got, f.Process(in[start:end])...)
		}
		if len(got) != len(whole) {
			t.Fatalf("chunk %d: got %d outputs, want %d", chunk, len(got), len(whole))
		}
		for i := range whole {
			if math.Abs(got[i]-whole[i]) > tol {
				t.Fatalf("chunk %d: output %d = %g, want %g", chunk, i, got[i], whole[i])
			}
		}
	}
}

func TestXlatingFilterMatchesDirectConvolution(t *testing.T) {
	taps := NewLowPassFIR(8000, 960000, 45)
	in := complexRamp(2048)
	decim := 30

	f := NewXlatingFilter(0, taps, decim, 960000)
	got := append([]complex128(nil), f.Process(in)...)
	want := naiveComplexFIR(in, taps, decim)

	if len(got) != len(want) {
		t.Fatalf("got %d outputs, want %d", len(got), len(want))
	}
	for i := range want {
		if cmplx.Abs(got[i]-want[i]) > tol {
			t.Fatalf("output %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestXlatingFilterIsBlockSizeInvariant(t *testing.T) {
	taps := NewLowPassFIR(8000, 960000, 45)
	in := complexRamp(1920)
	decim := 30

	whole := append([]complex128(nil), NewXlatingFilter(0, taps, decim, 960000).Process(in)...)

	for _, chunk := range []int{31, 96, 640, 1919} {
		f := NewXlatingFilter(0, taps, decim, 960000)
		var got []complex128
		for start := 0; start < len(in); start += chunk {
			end := min(start+chunk, len(in))
			got = append(got, f.Process(in[start:end])...)
		}
		if len(got) != len(whole) {
			t.Fatalf("chunk %d: got %d outputs, want %d", chunk, len(got), len(whole))
		}
		for i := range whole {
			if cmplx.Abs(got[i]-whole[i]) > tol {
				t.Fatalf("chunk %d: output %d = %v, want %v", chunk, i, got[i], whole[i])
			}
		}
	}
}

// A nonzero offset must still translate the band it is pointed at, and must not
// regress into the real-tap fast path.
func TestXlatingFilterWithOffsetShiftsToBaseband(t *testing.T) {
	const (
		sampleRate = 96000.0
		offset     = 12000.0
		n          = 4096
	)
	taps := NewLowPassFIR(4000, sampleRate, 65)
	in := make([]complex128, n)
	for i := range in {
		in[i] = cmplx.Rect(1, 2*math.Pi*offset*float64(i)/sampleRate)
	}

	f := NewXlatingFilter(offset, taps, 4, sampleRate)
	out := f.Process(in)

	// After translation the tone sits at DC: consecutive outputs share a phase,
	// so the mean magnitude survives while the phase difference vanishes.
	tail := out[len(out)/2:]
	var meanMag float64
	for _, s := range tail {
		meanMag += cmplx.Abs(s)
	}
	meanMag /= float64(len(tail))
	if meanMag < 0.5 {
		t.Fatalf("translated tone was attenuated: mean magnitude %g", meanMag)
	}
	for i := 1; i < len(tail); i++ {
		if d := math.Abs(cmplx.Phase(tail[i] * cmplx.Conj(tail[i-1]))); d > 1e-6 {
			t.Fatalf("output %d not at DC: phase step %g rad", i, d)
		}
	}
}

func TestResetClearsHistory(t *testing.T) {
	taps := NewLowPassFIR(2000, 16000, 31)
	loud := make([]float64, 256)
	for i := range loud {
		loud[i] = 1
	}
	quiet := make([]float64, 256)

	f := NewRealFIRDecimator(taps, 2)
	f.Process(loud)
	f.Reset()
	got := f.Process(quiet)

	fresh := NewRealFIRDecimator(taps, 2).Process(quiet)
	if len(got) != len(fresh) {
		t.Fatalf("got %d outputs, want %d", len(got), len(fresh))
	}
	for i := range fresh {
		if math.Abs(got[i]-fresh[i]) > tol {
			t.Fatalf("output %d = %g after reset, want %g", i, got[i], fresh[i])
		}
	}
}

func TestCU8ConversionReusesBufferAndMatchesScale(t *testing.T) {
	raw := []byte{0, 0, 255, 255, 128, 127, 127, 128}
	got := CU8ToComplexInto(nil, raw)

	want := []complex128{
		complex(-127.5/128, -127.5/128),
		complex(127.5/128, 127.5/128),
		complex(0.5/128, -0.5/128),
		complex(-0.5/128, 0.5/128),
	}
	for i := range want {
		if cmplx.Abs(got[i]-want[i]) > 1e-15 {
			t.Fatalf("sample %d = %v, want %v", i, got[i], want[i])
		}
	}

	// Reuse must not reallocate, and must resize to the new (shorter) length.
	reused := CU8ToComplexInto(got, raw[:4])
	if len(reused) != 2 {
		t.Fatalf("got length %d, want 2", len(reused))
	}
	if &reused[0] != &got[0] {
		t.Error("expected the caller's buffer to be reused")
	}
}

func BenchmarkXlatingFilterNFM(b *testing.B) {
	// The production shape: 960 kS/s, 16 kHz channel, 20 ms blocks.
	taps := NewLowPassFIR(8000, 960000, 145)
	f := NewXlatingFilter(0, taps, 30, 960000)
	in := complexRamp(19200)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		f.Process(in)
	}
}
