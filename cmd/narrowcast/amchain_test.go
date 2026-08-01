package main

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/pierr3/narrowcast/pkg/config"
	"github.com/pierr3/narrowcast/pkg/protocol"
)

// End-to-end measurement of the AM receive chain: synthetic RF in, audio out,
// output SNR reported. This is the only way to answer "is the channel too wide"
// with a number instead of an opinion — the hiss a listener complains about is
// output SNR, and every stage between antenna and speaker moves it.

const (
	amTestRate  = 960_000 // SDR rate, as configured
	amToneHz    = 1000.0  // modulating tone
	amBlockLen  = 19200   // 20 ms of IQ at 960 kS/s, as the SDR delivers
	amAudioRate = 16000
)

// amSignal builds one block of CU8-scale complex IQ: a DSB-AM carrier at
// carrierOffsetHz from centre, modulated by a 1 kHz tone, plus complex AWGN.
//
// noiseSigma is per-component standard deviation. Carrier amplitude is fixed at
// 1, so the channel SNR is set entirely by noiseSigma.
func amSignal(n int, phase *float64, modPhase *float64, carrierOffsetHz, noiseSigma, depth float64, rng *rand.Rand) []complex128 {
	out := make([]complex128, n)
	for i := range out {
		*modPhase += 2 * math.Pi * amToneHz / amTestRate
		*phase += 2 * math.Pi * carrierOffsetHz / amTestRate
		env := 1 + depth*math.Sin(*modPhase)
		re := env*math.Cos(*phase) + rng.NormFloat64()*noiseSigma
		im := env*math.Sin(*phase) + rng.NormFloat64()*noiseSigma
		out[i] = complex(re, im)
	}
	return out
}

// goertzel returns the power at freqHz in samples.
func goertzel(samples []float64, freqHz, sampleRate float64) float64 {
	k := 2 * math.Cos(2*math.Pi*freqHz/sampleRate)
	var s0, s1, s2 float64
	for _, v := range samples {
		s0 = v + k*s1 - s2
		s2, s1 = s1, s0
	}
	return s1*s1 + s2*s2 - k*s1*s2
}

func totalPower(samples []float64) float64 {
	var sum float64
	for _, v := range samples {
		sum += v * v
	}
	return sum
}

// measureAMSNR runs a noisy AM signal through a real chain and returns the
// output SNR in dB: tone power against everything else in the audio band.
func measureAMSNR(t *testing.T, halfBandwidthHz, noiseSigma, carrierOffsetHz float64) float64 {
	t.Helper()

	cfg := config.DefaultConfig()
	cfg.SampleRate = amTestRate
	cfg.AMHalfBandwidthHz = halfBandwidthHz
	// Gate held open: this measures the demodulator, not the squelch.
	cfg.SquelchDBm = -200

	chain, err := buildDSPChain(protocol.ModeAM, cfg, cfg.OpusBitrate)
	if err != nil {
		t.Fatal(err)
	}

	rng := rand.New(rand.NewSource(1))
	var phase, modPhase float64
	var audio []float64

	// 40 blocks = 800 ms. The first half is discarded so filter history, the DC
	// blocker, the AGC and the carrier tracker have all settled.
	const blocks = 40
	for b := 0; b < blocks; b++ {
		iq := amSignal(amBlockLen, &phase, &modPhase, carrierOffsetHz, noiseSigma, 0.7, rng)
		out, _, open := chain.demodBlock(iq, float64(cfg.SquelchDBm), 125_500_000, testClock(b))
		if !open {
			t.Fatalf("block %d: squelch closed with the threshold at -200 dBm", b)
		}
		if b >= blocks/2 {
			audio = append(audio, out...)
		}
	}

	tone := goertzel(audio, amToneHz, amAudioRate)
	// Goertzel's power is scaled by N/2 relative to the per-sample sum used for
	// total power; normalise so the difference is the real noise.
	toneP := tone * 2 / float64(len(audio))
	noiseP := totalPower(audio) - toneP
	if noiseP <= 0 {
		t.Fatalf("degenerate measurement: tone %.3g, total %.3g", toneP, totalPower(audio))
	}
	return 10 * math.Log10(toneP/noiseP)
}

// The chain's clock only drives the carrier-scan interval; a synthetic one
// keeps the measurement deterministic.
func testClock(block int) time.Time {
	return time.Unix(0, 0).Add(time.Duration(block) * 20 * time.Millisecond)
}

// The question behind "there's still a lot of white noise": how much output SNR
// does narrowing the AM filter actually buy? Envelope-detected noise power
// scales with the bandwidth admitted, so this should improve as the filter
// tightens — right up until the filter starts eating the sidebands that carry
// the voice.
func TestAMFilterWidthVsNoise(t *testing.T) {
	// ~10 dB channel SNR in the wide channel: a weak-but-readable signal, which
	// is where hiss is actually a complaint.
	const noiseSigma = 0.25

	for _, halfBW := range []float64{5000, 4000, 3500, 3000, 2500, 2000} {
		snr := measureAMSNR(t, halfBW, noiseSigma, 0)
		t.Logf("half-bandwidth %5.0f Hz → output SNR %6.2f dB", halfBW, snr)
	}
}

// Same sweep against an offset carrier, the case the tracker exists for. A
// narrow filter must not start clipping the carrier it has centred.
func TestAMFilterWidthVsNoiseOffsetCarrier(t *testing.T) {
	const noiseSigma = 0.25

	for _, halfBW := range []float64{3500, 3000, 2500} {
		snr := measureAMSNR(t, halfBW, noiseSigma, -7500)
		t.Logf("offset carrier, half-bandwidth %5.0f Hz → output SNR %6.2f dB", halfBW, snr)
	}
}

// How output SNR tracks channel SNR. Envelope detection is non-coherent, so it
// degrades faster than linearly once the carrier stops dominating the noise —
// the classic AM threshold effect — and that is the regime a listener describes
// as hiss. Establishes what a better detector would have to beat.
func TestAMOutputSNRVsChannelSNR(t *testing.T) {
	for _, sigma := range []float64{0.05, 0.1, 0.25, 0.5, 1.0, 2.0} {
		// Carrier amplitude is 1 by construction, and noise power is 2*sigma^2
		// across the two components.
		channelSNR := 10 * math.Log10(1/(2*sigma*sigma))
		out := measureAMSNR(t, 3500, sigma, 0)
		t.Logf("sigma %.2f (carrier/noise %6.2f dB in the wide channel) → output SNR %6.2f dB",
			sigma, channelSNR, out)
	}
}
