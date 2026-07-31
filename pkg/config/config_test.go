package config

import (
	"strings"
	"testing"

	"github.com/pierr3/narrowcast/pkg/protocol"
)

func TestDefaultConfigIsValid(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}
}

// The DSP chain derives decimation with integer division, so every mode's audio
// rate has to divide the sample rate exactly or audio comes out clocked wrong
// with nothing logged. 1.024 MS/s is the trap: fine for 16 kHz, off by 1.6 % for
// the 48 kHz WFM rate.
func TestValidateRejectsRatesThatDontDivideEveryAudioRate(t *testing.T) {
	cases := []struct {
		rate int
		ok   bool
	}{
		{960_000, true},
		{1_440_000, true},
		{1_920_000, true},
		{2_400_000, true},
		{1_024_000, false}, // 1024000/48000 = 21.33
		{1_000_000, false},
		{2_048_000, false},
	}

	for _, tc := range cases {
		cfg := DefaultConfig()
		cfg.SampleRate = tc.rate
		err := cfg.Validate()
		if tc.ok && err != nil {
			t.Errorf("samplerate %d rejected: %v", tc.rate, err)
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("samplerate %d accepted but does not divide every audio rate", tc.rate)
				continue
			}
			if !strings.Contains(err.Error(), "multiple") {
				t.Errorf("samplerate %d: unhelpful error %q", tc.rate, err)
			}
		}
	}
}

func TestValidateEnforcesNyquistForEveryMode(t *testing.T) {
	cfg := DefaultConfig()
	// Divides both audio rates, but only 96 kHz wide — below Nyquist for the
	// 200 kHz WFM channel, which the user could switch to at runtime.
	cfg.SampleRate = 96_000
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected a Nyquist error for the WFM channel bandwidth")
	}
	if !strings.Contains(err.Error(), protocol.ModeWFM.String()) {
		t.Errorf("error should name the offending mode, got %q", err)
	}
}

func TestValidateChecksFFTParameters(t *testing.T) {
	cases := map[string]func(*Config){
		"non-power-of-two fftsize": func(c *Config) { c.FFTSize = 1000 },
		"fftsize below minimum":    func(c *Config) { c.FFTSize = 1 },
		"fftbins above fftsize":    func(c *Config) { c.FFTBins = c.FFTSize + 1 },
		"fftbins below one":        func(c *Config) { c.FFTBins = 0 },
		"fftrate below one":        func(c *Config) { c.FFTRate = 0 },
		"opus bitrate too low":     func(c *Config) { c.OpusBitrate = 2000 },
		"negative samplerate":      func(c *Config) { c.SampleRate = -1 },
	}

	for name, mutate := range cases {
		cfg := DefaultConfig()
		mutate(cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
