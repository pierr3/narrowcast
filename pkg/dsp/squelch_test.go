package dsp

import (
	"math"
	"math/cmplx"
	"testing"
)

// Channel rate used by AM at the default sample rate.
const chRate = 48000.0

// blockSamples is one 20 ms block at the channel rate.
const blockSamples = int(chRate * 0.020)

func TestChannelPowerDbMatchesAmplitude(t *testing.T) {
	// A full-scale constant carrier is 0 dBFS.
	full := make([]complex128, 64)
	for i := range full {
		full[i] = cmplx.Rect(1, float64(i)*0.3)
	}
	if got := ChannelPowerDb(full); math.Abs(got) > 1e-9 {
		t.Errorf("unit carrier = %g dB, want 0", got)
	}

	// Halving amplitude is -6 dB.
	half := make([]complex128, 64)
	for i, s := range full {
		half[i] = s * 0.5
	}
	if got := ChannelPowerDb(half); math.Abs(got+6.0206) > 1e-3 {
		t.Errorf("half-amplitude carrier = %g dB, want ≈ -6.02", got)
	}

	if got := ChannelPowerDb(nil); got > -100 {
		t.Errorf("empty block = %g dB, want a floor value", got)
	}
}

// The regression this whole type exists for: a speech-shaped dip in level must
// not close the gate mid-transmission.
func TestSquelchHoldsThroughSpeechDips(t *testing.T) {
	// Threshold -50, hysteresis 6 dB, hang 500 ms.
	s := NewSquelch(-50, 6, 500, chRate)

	// Signal arrives comfortably above the threshold.
	for i := 0; i < 10; i++ {
		s.Update(-40, blockSamples)
	}
	if !s.IsOpen() {
		t.Fatal("gate should be open on a signal 10 dB above threshold")
	}

	// Speech dips to just below the threshold for 200 ms (10 blocks) — the
	// case that used to chop transmissions apart.
	for i := 0; i < 10; i++ {
		if !s.Update(-53, blockSamples) {
			t.Fatalf("gate closed during a 3 dB dip at block %d", i)
		}
	}

	// And recovers.
	if !s.Update(-40, blockSamples) {
		t.Fatal("gate should still be open after the dip")
	}
}

func TestSquelchClosesWhenSignalReallyGoes(t *testing.T) {
	s := NewSquelch(-50, 6, 200, chRate)
	for i := 0; i < 10; i++ {
		s.Update(-40, blockSamples)
	}

	// Drop far below the close threshold; the gate must survive the hang time
	// and then shut.
	closedAfter := -1
	for i := 0; i < 40; i++ {
		if !s.Update(-90, blockSamples) {
			closedAfter = i
			break
		}
	}
	if closedAfter < 0 {
		t.Fatal("gate never closed on a dead channel")
	}
	// 200 ms of hang at 20 ms per block, plus a few blocks for the decay
	// smoothing to bring the level down.
	if closedAfter < 8 {
		t.Errorf("closed after %d blocks — hang time too short", closedAfter)
	}
	if closedAfter > 30 {
		t.Errorf("closed after %d blocks — hang time far too long", closedAfter)
	}
}

// Noise sitting exactly on the threshold is the classic chatter case.
func TestSquelchDoesNotChatterOnThresholdNoise(t *testing.T) {
	s := NewSquelch(-50, 6, 300, chRate)
	for i := 0; i < 5; i++ {
		s.Update(-40, blockSamples)
	}

	transitions := 0
	prev := s.IsOpen()
	for i := 0; i < 60; i++ {
		// Wobble ±2 dB around the threshold, inside the hysteresis band.
		level := -50.0
		if i%2 == 0 {
			level = -52
		} else {
			level = -48
		}
		open := s.Update(level, blockSamples)
		if open != prev {
			transitions++
			prev = open
		}
	}
	if transitions != 0 {
		t.Errorf("gate toggled %d times while noise wobbled inside the hysteresis band", transitions)
	}
}

func TestSquelchOpensPromptlyOnSignal(t *testing.T) {
	s := NewSquelch(-50, 6, 300, chRate)
	for i := 0; i < 20; i++ {
		s.Update(-90, blockSamples)
	}
	if s.IsOpen() {
		t.Fatal("gate should be closed on a dead channel")
	}
	// A transmission starting should be caught within a block or two, or the
	// first syllable is lost.
	opened := -1
	for i := 0; i < 5; i++ {
		if s.Update(-30, blockSamples) {
			opened = i
			break
		}
	}
	if opened < 0 || opened > 1 {
		t.Errorf("gate opened after %d blocks, want ≤ 1", opened)
	}
}

func TestSquelchResetClearsState(t *testing.T) {
	s := NewSquelch(-50, 6, 300, chRate)
	for i := 0; i < 10; i++ {
		s.Update(-30, blockSamples)
	}
	s.Reset()
	if s.IsOpen() {
		t.Error("gate should be closed after Reset")
	}
	// Reset must also drop the smoothed level, or a loud pre-retune signal
	// would hold the gate open on the new frequency.
	if s.Update(-90, blockSamples) {
		t.Error("gate opened on a dead channel straight after Reset")
	}
}

// Dragging the slider above the signal must mute it, even though the signal
// hasn't moved and is still inside the hysteresis band.
func TestSquelchThresholdChangeBeatsHysteresis(t *testing.T) {
	s := NewSquelch(-50, 6, 500, chRate)
	for i := 0; i < 10; i++ {
		s.Update(-40, blockSamples)
	}
	if !s.IsOpen() {
		t.Fatal("expected gate open")
	}

	// Raise the threshold just above the signal — inside the 6 dB band.
	s.SetThreshold(-38)
	if s.IsOpen() {
		t.Error("gate stayed open after the threshold was raised above the signal")
	}
	if s.Update(-40, blockSamples) {
		t.Error("gate reopened on a signal below the new threshold")
	}

	// Repeated no-op calls must not disturb a settled gate: the pipeline calls
	// SetThreshold every block.
	s.SetThreshold(-60)
	for i := 0; i < 5; i++ {
		s.Update(-40, blockSamples)
	}
	for i := 0; i < 20; i++ {
		s.SetThreshold(-60)
		if !s.Update(-57, blockSamples) { // inside the band, signal present
			t.Fatalf("gate closed at block %d despite an unchanged threshold", i)
		}
	}
}

func TestSquelchThresholdIsLive(t *testing.T) {
	s := NewSquelch(-50, 6, 300, chRate)
	for i := 0; i < 10; i++ {
		s.Update(-60, blockSamples)
	}
	if s.IsOpen() {
		t.Fatal("gate should be closed below threshold")
	}
	s.SetThreshold(-70) // user drags the squelch down
	if !s.Update(-60, blockSamples) {
		t.Error("gate should open once the threshold moves below the signal")
	}
}
