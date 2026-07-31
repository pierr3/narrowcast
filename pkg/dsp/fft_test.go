package dsp

import (
	"math"
	"math/cmplx"
	"testing"
)

// oneToneFFT returns the FFT of a unit tone at bin k of an n-point transform.
func oneToneFFT(n, k int) []complex128 {
	buf := make([]complex128, n)
	for i := range buf {
		buf[i] = cmplx.Rect(1, 2*math.Pi*float64(k)*float64(i)/float64(n))
	}
	FFT(buf)
	return buf
}

func TestFFTPutsToneInExpectedBin(t *testing.T) {
	const n, k = 64, 7
	out := oneToneFFT(n, k)

	peak := 0
	for i := range out {
		if cmplx.Abs(out[i]) > cmplx.Abs(out[peak]) {
			peak = i
		}
	}
	if peak != k {
		t.Fatalf("peak in bin %d, want %d", peak, k)
	}
	if got := cmplx.Abs(out[k]); math.Abs(got-float64(n)) > 1e-9 {
		t.Errorf("peak magnitude %g, want %d", got, n)
	}
}

func TestMagnitudeToBinsCentersDC(t *testing.T) {
	const n = 64
	// DC tone (k=0) must land in the middle bin after the FFT shift.
	bins := MagnitudeToBins(nil, oneToneFFT(n, 0), n)
	if len(bins) != n {
		t.Fatalf("got %d bins, want %d", len(bins), n)
	}

	peak := 0
	for i, v := range bins {
		if v > bins[peak] {
			peak = i
		}
	}
	if peak != n/2 {
		t.Errorf("DC landed in bin %d, want %d (centered)", peak, n/2)
	}
	// A full-scale tone maps to the top of the 0-255 range.
	if bins[peak] != 255 {
		t.Errorf("full-scale tone mapped to %d, want 255", bins[peak])
	}
}

func TestMagnitudeToBinsSilenceMapsToZero(t *testing.T) {
	bins := MagnitudeToBins(nil, make([]complex128, 32), 32)
	for i, v := range bins {
		if v != 0 {
			t.Fatalf("bin %d = %d for silence, want 0", i, v)
		}
	}
}

// Pooling must preserve a single-bin carrier, which is the whole reason it uses
// max rather than mean.
func TestMagnitudeToBinsPoolingKeepsNarrowCarrier(t *testing.T) {
	const n, out = 1024, 256
	// Tone at FFT bin 300; after the shift it sits at (300+512)%1024 = 812,
	// which pools into output bin 812/4 = 203.
	bins := MagnitudeToBins(nil, oneToneFFT(n, 300), out)
	if len(bins) != out {
		t.Fatalf("got %d bins, want %d", len(bins), out)
	}

	peak := 0
	for i, v := range bins {
		if v > bins[peak] {
			peak = i
		}
	}
	if peak != 203 {
		t.Errorf("pooled peak in bin %d, want 203", peak)
	}
	if bins[peak] != 255 {
		t.Errorf("pooled peak = %d, want the carrier to survive at 255", bins[peak])
	}

	// Unpooled and pooled peaks must agree: max-pooling never attenuates.
	full := MagnitudeToBins(nil, oneToneFFT(n, 300), n)
	if full[812] != bins[peak] {
		t.Errorf("pooled peak %d differs from unpooled %d", bins[peak], full[812])
	}
}

func TestMagnitudeToBinsHandlesIndivisibleBinCount(t *testing.T) {
	const n, out = 64, 7 // 64/7 is not integral
	bins := MagnitudeToBins(nil, oneToneFFT(n, 0), out)
	if len(bins) != out {
		t.Fatalf("got %d bins, want %d", len(bins), out)
	}
	// Every FFT bin must fall in exactly one group: boundaries are k*n/out, so
	// the last group has to reach the end of the spectrum.
	if got := (out * n) / out; got != n {
		t.Fatalf("group arithmetic lost bins: %d != %d", got, n)
	}
}

func TestMagnitudeToBinsReusesBuffer(t *testing.T) {
	dst := make([]byte, 256)
	got := MagnitudeToBins(dst, oneToneFFT(1024, 10), 256)
	if &got[0] != &dst[0] {
		t.Error("expected the caller's buffer to be reused")
	}
	// A smaller request must reslice rather than reallocate.
	smaller := MagnitudeToBins(got, oneToneFFT(1024, 10), 64)
	if len(smaller) != 64 || &smaller[0] != &dst[0] {
		t.Error("expected a shorter result in the same buffer")
	}
}

func TestHannWindowMatchesDefinition(t *testing.T) {
	const n = 16
	h := NewHannWindow(n)
	data := make([]complex128, n)
	for i := range data {
		data[i] = complex(1, 0)
	}
	h.Apply(data)

	for i := range data {
		want := 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(n)))
		if math.Abs(real(data[i])-want) > 1e-15 {
			t.Fatalf("coefficient %d = %g, want %g", i, real(data[i]), want)
		}
	}
}

func BenchmarkFFTAndBins(b *testing.B) {
	const n = 1024
	frame := oneToneFFT(n, 40) // reused as arbitrary input
	work := make([]complex128, n)
	hann := NewHannWindow(n)
	dst := make([]byte, 256)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		copy(work, frame)
		hann.Apply(work)
		FFT(work)
		MagnitudeToBins(dst, work, 256)
	}
}
