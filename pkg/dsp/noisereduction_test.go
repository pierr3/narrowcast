package dsp

import (
	"math"
	"math/rand"
	"testing"
)

const nrRate = 16000

func nrTone(n int, hz, amp float64, phase *float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		*phase += 2 * math.Pi * hz / nrRate
		out[i] = amp * math.Sin(*phase)
	}
	return out
}

func addNoise(dst []float64, sigma float64, rng *rand.Rand) {
	for i := range dst {
		dst[i] += rng.NormFloat64() * sigma
	}
}

func rms(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	var s float64
	for _, v := range x {
		s += v * v
	}
	return math.Sqrt(s / float64(len(x)))
}

// train feeds noise-only blocks so the reducer learns the floor.
func train(nr *SpectralNR, sigma float64, rng *rand.Rand, blocks int) {
	for i := 0; i < blocks; i++ {
		b := make([]float64, 320)
		addNoise(b, sigma, rng)
		nr.Process(b, true)
	}
}

// The headline claim: hiss down, voice left standing.
func TestSpectralNRImprovesSNR(t *testing.T) {
	const sigma = 0.02
	for _, level := range []NRLevel{NRLight, NRMedium, NRStrong} {
		rng := rand.New(rand.NewSource(4))
		nr := NewSpectralNR(level)
		train(nr, sigma, rng, 60)
		if !nr.Ready() {
			t.Fatal("did not learn the noise floor")
		}

		// Measure the residual noise on noise-only input...
		var noiseOut []float64
		for i := 0; i < 40; i++ {
			b := make([]float64, 320)
			addNoise(b, sigma, rng)
			noiseOut = append(noiseOut, nr.Process(b, false)...)
		}
		// ...and the surviving tone on speech-like input at the same noise.
		nr2 := NewSpectralNR(level)
		rng2 := rand.New(rand.NewSource(4))
		train(nr2, sigma, rng2, 60)
		var phase float64
		var voiceOut []float64
		for i := 0; i < 40; i++ {
			b := nrTone(320, 800, 0.2, &phase)
			addNoise(b, sigma, rng2)
			voiceOut = append(voiceOut, nr2.Process(b, false)...)
		}

		// Reference: same signals with NR off.
		refNoise := make([]float64, 0, len(noiseOut))
		refVoice := make([]float64, 0, len(voiceOut))
		rng3 := rand.New(rand.NewSource(4))
		var p2 float64
		for i := 0; i < 40; i++ {
			b := make([]float64, 320)
			addNoise(b, sigma, rng3)
			refNoise = append(refNoise, b...)
			v := nrTone(320, 800, 0.2, &p2)
			addNoise(v, sigma, rng3)
			refVoice = append(refVoice, v...)
		}

		noiseCut := 20 * math.Log10(rms(refNoise)/math.Max(rms(noiseOut), 1e-12))
		voiceCut := 20 * math.Log10(rms(refVoice)/math.Max(rms(voiceOut), 1e-12))
		t.Logf("%v: noise -%.1f dB, voice -%.1f dB, net SNR gain %.1f dB",
			level, noiseCut, voiceCut, noiseCut-voiceCut)

		if noiseCut < 4 {
			t.Errorf("%v only removed %.1f dB of noise", level, noiseCut)
		}
		if voiceCut > 3 {
			t.Errorf("%v attenuated the voice by %.1f dB", level, voiceCut)
		}
	}
}

// Off must be bit-exact passthrough, so the setting is a real escape hatch.
func TestSpectralNROffIsBypass(t *testing.T) {
	nr := NewSpectralNR(NROff)
	in := []float64{0.1, -0.2, 0.3, 0.4}
	out := nr.Process(in, false)
	if len(out) != len(in) {
		t.Fatalf("got %d samples, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("sample %d changed: %v → %v", i, in[i], out[i])
		}
	}
}

// Subtracting a half-learned estimate sounds worse than doing nothing, so it
// must not start early.
func TestSpectralNRWaitsUntilItHasLearned(t *testing.T) {
	nr := NewSpectralNR(NRMedium)
	rng := rand.New(rand.NewSource(9))
	if nr.Ready() {
		t.Fatal("claims readiness before seeing any noise")
	}
	train(nr, 0.02, rng, 3)
	if nr.Ready() {
		t.Error("ready after 3 blocks of noise")
	}
	train(nr, 0.02, rng, 60)
	if !nr.Ready() {
		t.Error("still not ready after 60 blocks")
	}
}

// Sample count must be conserved in steady state, or audio drifts against the
// Opus frame clock.
func TestSpectralNRConservesSampleRate(t *testing.T) {
	nr := NewSpectralNR(NRMedium)
	rng := rand.New(rand.NewSource(11))
	train(nr, 0.02, rng, 60)

	var in, out int
	for i := 0; i < 200; i++ {
		b := make([]float64, 320)
		addNoise(b, 0.02, rng)
		in += len(b)
		out += len(nr.Process(b, false))
	}
	// One window of startup latency is expected; beyond that they must match.
	if diff := in - out; diff < 0 || diff > nrFFTSize {
		t.Errorf("in %d, out %d (difference %d, want 0..%d)", in, out, diff, nrFFTSize)
	}
}

// Overlap-add with a periodic Hann at 50% must reconstruct exactly when no gain
// is applied. If this drifts the audio gets amplitude ripple at the hop rate.
func TestSpectralNROverlapAddReconstructs(t *testing.T) {
	nr := NewSpectralNR(NRMedium)
	// Learned but with a zero noise estimate: gains all clamp to 1, so the path
	// is analyse → identity → synthesise.
	nr.learned = nrMinLearnFrames

	var phase float64
	var out []float64
	for i := 0; i < 30; i++ {
		out = append(out, nr.Process(nrTone(320, 700, 0.5, &phase), false)...)
	}
	// Skip startup, then the result should be a clean tone of the same
	// amplitude.
	tail := out[1024:]
	if got := rms(tail); math.Abs(got-0.5/math.Sqrt2) > 0.02 {
		t.Errorf("reconstructed RMS %.4f, want %.4f", got, 0.5/math.Sqrt2)
	}
}

func TestSpectralNRResetClearsLearning(t *testing.T) {
	nr := NewSpectralNR(NRMedium)
	rng := rand.New(rand.NewSource(13))
	train(nr, 0.02, rng, 60)
	if !nr.Ready() {
		t.Fatal("not ready")
	}
	nr.Reset()
	if nr.Ready() {
		t.Error("still ready after Reset")
	}
}

func BenchmarkSpectralNR(b *testing.B) {
	nr := NewSpectralNR(NRMedium)
	rng := rand.New(rand.NewSource(1))
	train(nr, 0.02, rng, 60)
	block := make([]float64, 320) // one 20 ms block at 16 kHz
	addNoise(block, 0.02, rng)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		nr.Process(block, false)
	}
}
