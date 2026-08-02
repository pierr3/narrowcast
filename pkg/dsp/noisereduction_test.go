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
//
// Speech is delivered in bursts rather than as a continuous tone, because that
// is what speech is and because minimum statistics depends on it — see
// TestSpectralNRSuppressesASteadyTone.
func TestSpectralNRImprovesSNR(t *testing.T) {
	const sigma = 0.02
	for _, level := range []NRLevel{NRLight, NRMedium, NRStrong} {
		nr := NewSpectralNR(level)
		rng := rand.New(rand.NewSource(4))
		var phase float64
		var out, ref []float64
		var speechIdx []int // sample ranges that contain the tone

		for cycle := 0; cycle < 24; cycle++ {
			for i := 0; i < 12; i++ { // 240 ms of speech
				b := nrTone(320, 800, 0.2, &phase)
				addNoise(b, sigma, rng)
				if cycle >= 12 {
					speechIdx = append(speechIdx, len(ref))
				}
				ref = append(ref, b...)
				out = append(out, nr.Process(b, false)...)
			}
			for i := 0; i < 8; i++ { // 160 ms of gap
				b := make([]float64, 320)
				addNoise(b, sigma, rng)
				ref = append(ref, b...)
				out = append(out, nr.Process(b, false)...)
			}
		}

		// Gaps in the second half: pure noise, so this is the reduction.
		gap := len(out) - 8*320 + 640
		noiseCut := 20 * math.Log10(rms(ref[gap:])/math.Max(rms(out[gap:]), 1e-12))

		// Speech blocks in the second half: how much of the voice survived.
		var inS, outS []float64
		for _, at := range speechIdx {
			if at+320 <= len(out) {
				inS = append(inS, ref[at:at+320]...)
				outS = append(outS, out[at:at+320]...)
			}
		}
		voiceCut := 20 * math.Log10(rms(inS)/math.Max(rms(outS), 1e-12))
		t.Logf("%v: noise -%.1f dB, voice -%.1f dB, net SNR gain %.1f dB",
			level, noiseCut, voiceCut, noiseCut-voiceCut)

		if noiseCut < 6 {
			t.Errorf("%v only removed %.1f dB of noise", level, noiseCut)
		}
		if voiceCut > 2 {
			t.Errorf("%v attenuated the voice by %.1f dB", level, voiceCut)
		}
	}
}

// A tone that never stops is, over the tracker's window, indistinguishable from
// stationary noise, and gets attenuated. That is intended: on the airband a
// permanent tone is a heterodyne or a stuck carrier, not information, so
// notching it is a bonus. Documented as a test so nobody later reads it as a
// bug and "fixes" it by lengthening the window until speech starts vanishing.
func TestSpectralNRSuppressesASteadyTone(t *testing.T) {
	nr := NewSpectralNR(NRMedium)
	var phase float64
	var out, ref []float64
	for i := 0; i < 200; i++ {
		b := nrTone(320, 1200, 0.2, &phase)
		ref = append(ref, b...)
		out = append(out, nr.Process(b, false)...)
	}
	tail := len(out) - 20*320
	cut := 20 * math.Log10(rms(ref[tail:])/math.Max(rms(out[tail:]), 1e-12))
	t.Logf("steady tone attenuated by %.1f dB", cut)
	if cut < 1 {
		t.Errorf("steady tone survived untouched (%.1f dB) — minimum tracking is not running", cut)
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

// Overlap-add with a periodic Hann at 50% must reconstruct exactly while no
// gain is being applied. If it drifts, the audio gets amplitude ripple at the
// hop rate.
//
// Measured before the minimum tracker has filled a sub-window, because after
// that it legitimately starts attenuating: a tone that never stops is, over a
// 1.6 s window, indistinguishable from stationary noise, and suppressing it is
// the correct call. Speech is not stationary; a heterodyne whistle is, and
// having it notched out is a bonus rather than a defect.
func TestSpectralNROverlapAddReconstructs(t *testing.T) {
	nr := NewSpectralNR(NRMedium)
	var phase float64
	var out []float64
	// 15 blocks is 37 hops, inside the 48-frame first sub-window.
	for i := 0; i < 15; i++ {
		out = append(out, nr.Process(nrTone(320, 700, 0.5, &phase), false)...)
	}
	tail := out[512:] // past the one-window startup transient
	if got := rms(tail); math.Abs(got-0.5/math.Sqrt2) > 0.02 {
		t.Errorf("reconstructed RMS %.4f, want %.4f", got, 0.5/math.Sqrt2)
	}
}

// The fix that matters: learn the noise floor from the gaps inside speech,
// having never been shown a noise-only frame.
//
// The first implementation learned only while the squelch was shut, and on AM
// that is the wrong measurement — no carrier means envelope-detected noise with
// different statistics from the noise riding on a carrier during a
// transmission. On real air it delivered 2.6 dB where the bench predicted 13.
func TestSpectralNRLearnsFromGapsWithoutDeadAir(t *testing.T) {
	const sigma = 0.02
	rng := rand.New(rand.NewSource(21))
	nr := NewSpectralNR(NRMedium)

	// Speech-like: 300 ms of tone, 200 ms of gap, repeating. learnNoise is
	// never set — the squelch stays open throughout, as it does mid-QSO.
	var phase float64
	var out, ref []float64
	for cycle := 0; cycle < 20; cycle++ {
		for i := 0; i < 15; i++ { // 300 ms of speech
			b := nrTone(320, 800, 0.2, &phase)
			addNoise(b, sigma, rng)
			ref = append(ref, b...)
			out = append(out, nr.Process(b, false)...)
		}
		for i := 0; i < 10; i++ { // 200 ms of gap: noise only
			b := make([]float64, 320)
			addNoise(b, sigma, rng)
			ref = append(ref, b...)
			out = append(out, nr.Process(b, false)...)
		}
	}
	if !nr.Ready() {
		t.Fatal("never formed an estimate from gaps alone")
	}

	// Compare the last few gaps: input noise against output noise.
	gapStart := len(out) - 10*320 + 640
	inGap := rms(ref[gapStart:])
	outGap := rms(out[gapStart:])
	cut := 20 * math.Log10(inGap/math.Max(outGap, 1e-12))
	t.Logf("noise in gaps cut by %.1f dB, having only ever seen open-squelch audio", cut)
	if cut < 6 {
		t.Errorf("only %.1f dB of gap noise removed", cut)
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
