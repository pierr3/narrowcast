package dsp

import (
	"math"
	"testing"
)

const agcRate = 16000.0

// speechLike builds n samples of a tone at amp, as a stand-in for a steady
// vowel — enough for the AGC's envelope follower to settle on.
func speechLike(n int, amp float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = amp * math.Sin(2*math.Pi*500*float64(i)/agcRate)
	}
	return out
}

func peak(samples []float64) float64 {
	var p float64
	for _, v := range samples {
		if a := math.Abs(v); a > p {
			p = a
		}
	}
	return p
}

func newTestAGC() *AudioAGC {
	return NewAudioAGC(AMAGCTarget, 0.1, 0.001, 500.0, agcRate, 0)
}

// The AGC's whole job: whatever level arrives, speech leaves at roughly the
// target. The RF gain setting scales everything ahead of it by an arbitrary
// factor, so the output level must not depend on the input level.
//
// This is the regression behind "I set RF gain to 20 dB and now I can barely
// hear anything at full volume". The AGC used to freeze whenever the envelope
// fell below an absolute floor of 0.02, which a lower RF gain puts it under
// permanently — so it sat at unity gain on a small signal and never recovered,
// because the branch that raises gain was the one being skipped.
func TestAudioAGCReachesTargetAcrossInputLevels(t *testing.T) {
	// Four orders of magnitude of RF gain difference.
	for _, amp := range []float64{0.5, 0.1, 0.02, 0.005, 0.001} {
		agc := newTestAGC()
		var last []float64
		// 2 s of audio: long enough for gain to converge from 1.0.
		for block := 0; block < 100; block++ {
			buf := speechLike(320, amp)
			agc.Process(buf)
			last = buf
		}
		got := peak(last)
		if got < AMAGCTarget*0.5 || got > AMAGCTarget*1.5 {
			t.Errorf("input amplitude %.4f → output peak %.4f, want ≈%.2f",
				amp, got, AMAGCTarget)
		}
		t.Logf("input %.4f → output peak %.3f (gain ×%.0f)", amp, got, got/amp)
	}
}

// Gain must be bounded, or a near-silent input drives it somewhere absurd and
// the next real transmission arrives as a wall of clipping.
func TestAudioAGCGainIsBounded(t *testing.T) {
	agc := newTestAGC()
	for block := 0; block < 500; block++ {
		buf := speechLike(320, 1e-9)
		agc.Process(buf)
	}
	if agc.gain > maxAudioAGCGain*1.01 {
		t.Errorf("gain reached ×%.0f, want at most ×%.0f", agc.gain, maxAudioAGCGain)
	}
	t.Logf("near-silent input drove gain to ×%.0f (cap ×%.0f)", agc.gain, maxAudioAGCGain)
}

// Hang time is what stops the gain ramping into the noise during the pauses
// between words, so a short gap must not move it.
func TestAudioAGCHoldsGainThroughAShortGap(t *testing.T) {
	agc := newTestAGC()
	for block := 0; block < 100; block++ {
		buf := speechLike(320, 0.1)
		agc.Process(buf)
	}
	settled := agc.gain

	// 200 ms of near-silence, well inside the 500 ms hang.
	for block := 0; block < 10; block++ {
		buf := speechLike(320, 1e-6)
		agc.Process(buf)
	}
	if ratio := agc.gain / settled; ratio > 1.2 {
		t.Errorf("gain rose ×%.2f during a 200 ms gap — hang time is not holding", ratio)
	}
}

// Output must never exceed full scale, whatever the AGC is doing.
func TestAudioAGCNeverClipsPastFullScale(t *testing.T) {
	agc := newTestAGC()
	for block := 0; block < 50; block++ {
		buf := speechLike(320, 0.001)
		agc.Process(buf)
	}
	// A sudden loud transmission after the gain has wound up.
	buf := speechLike(320, 1.0)
	agc.Process(buf)
	if p := peak(buf); p > 1.0 {
		t.Errorf("output peaked at %.3f, past full scale", p)
	}
}
