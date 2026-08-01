package audio

import (
	"math"
	"testing"
)

const benchRate = 16000

// speech builds voiced-ish audio: a couple of harmonics plus a fricative-band
// tone. A pure sine is unrepresentative, since encode cost depends on how hard
// the signal is to model.
func speech(sampleRate, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		t := float64(i) / float64(sampleRate)
		out[i] = 0.4*math.Sin(2*math.Pi*220*t) +
			0.2*math.Sin(2*math.Pi*660*t) +
			0.05*math.Sin(2*math.Pi*3100*t)
	}
	return out
}

func newTestEncoder(t testing.TB, complexity int) *OpusEncoder {
	t.Helper()
	enc, err := NewOpusEncoder(benchRate, 32000, complexity)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func TestNewOpusEncoderAppliesComplexity(t *testing.T) {
	enc := newTestEncoder(t, 3)
	got, err := enc.Complexity()
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Errorf("complexity = %d, want 3", got)
	}
}

// One 20 ms block at the audio rate is exactly one frame, so the common case
// must emit exactly one packet and leave nothing buffered.
func TestEncodeEmitsOneFramePerBlock(t *testing.T) {
	enc := newTestEncoder(t, 5)
	block := speech(benchRate, benchRate*20/1000)

	for i := 0; i < 5; i++ {
		packets, err := enc.Encode(block)
		if err != nil {
			t.Fatal(err)
		}
		if len(packets) != 1 {
			t.Fatalf("block %d produced %d packets, want 1", i, len(packets))
		}
		if len(packets[0]) == 0 {
			t.Fatalf("block %d produced an empty packet", i)
		}
	}
	if len(enc.buf) != 0 {
		t.Errorf("%d samples left buffered after whole frames", len(enc.buf))
	}
}

// A block that isn't a whole number of frames must carry the remainder over
// rather than dropping or duplicating it. This is what consume() gets right and
// a bare re-slice got wrong.
func TestEncodeCarriesPartialFrameOver(t *testing.T) {
	enc := newTestEncoder(t, 5)
	frame := enc.FrameSize()

	// Half a frame: nothing out, half buffered.
	if packets, err := enc.Encode(speech(benchRate, frame/2)); err != nil {
		t.Fatal(err)
	} else if len(packets) != 0 {
		t.Fatalf("half a frame produced %d packets, want 0", len(packets))
	}
	if len(enc.buf) != frame/2 {
		t.Fatalf("buffered %d samples, want %d", len(enc.buf), frame/2)
	}

	// Another block completes the first frame and leaves the rest.
	packets, err := enc.Encode(speech(benchRate, frame))
	if err != nil {
		t.Fatal(err)
	}
	if len(packets) != 1 {
		t.Fatalf("got %d packets, want 1", len(packets))
	}
	if len(enc.buf) != frame/2 {
		t.Errorf("carried %d samples over, want %d", len(enc.buf), frame/2)
	}
}

// Two frames in one call must not alias each other — they are both still
// referenced by the returned slice.
func TestEncodeDoesNotAliasPacketsWithinACall(t *testing.T) {
	enc := newTestEncoder(t, 5)
	packets, err := enc.Encode(speech(benchRate, 2*enc.FrameSize()))
	if err != nil {
		t.Fatal(err)
	}
	if len(packets) != 2 {
		t.Fatalf("got %d packets, want 2", len(packets))
	}
	if &packets[0][:1][0] == &packets[1][:1][0] {
		t.Error("both packets point at the same buffer")
	}
}

func TestResetDropsBufferedSamples(t *testing.T) {
	enc := newTestEncoder(t, 5)
	if _, err := enc.Encode(speech(benchRate, enc.FrameSize()/2)); err != nil {
		t.Fatal(err)
	}
	enc.Reset()
	if len(enc.buf) != 0 {
		t.Errorf("%d samples survived Reset", len(enc.buf))
	}
}

// Complexity is the pipeline's biggest lever and the reason the default is 5:
// on one 20 ms voice frame, 9 (libopus's own pick here) measured 119 µs against
// 55 µs for 5, which is comparable to the entire wideband channel filter.
func benchEncodeAt(b *testing.B, complexity int) {
	enc := newTestEncoder(b, complexity)
	block := speech(benchRate, benchRate*20/1000)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := enc.Encode(block); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOpusEncode10(b *testing.B) { benchEncodeAt(b, 10) }
func BenchmarkOpusEncode9(b *testing.B)  { benchEncodeAt(b, 9) }
func BenchmarkOpusEncode5(b *testing.B)  { benchEncodeAt(b, 5) }
func BenchmarkOpusEncode3(b *testing.B)  { benchEncodeAt(b, 3) }
func BenchmarkOpusEncode0(b *testing.B)  { benchEncodeAt(b, 0) }
