package config

import (
	"flag"
	"fmt"

	"github.com/pierr3/narrowcast/pkg/protocol"
)

type Config struct {
	// Network
	Host     string
	Port     int
	CertFile string
	KeyFile  string

	// SDR
	Simulate      bool
	SampleRate    int
	DeviceIndex   int
	DeviceSerial  string
	TunerGainAuto bool
	TunerGain     float64

	// Defaults
	FrequencyHz uint64
	DemodMode   protocol.DemodMode
	SquelchDBm  float32
	// SquelchHysteresisDb is how far below the open threshold the signal must
	// fall before the gate closes. Without it a threshold set near a signal
	// flutters open and shut on noise.
	SquelchHysteresisDb float64
	// SquelchHangMs keeps the gate open after the signal drops, bridging the
	// pauses between words and the brief dip as a transmitter unkeys.
	SquelchHangMs float64

	// DSP
	FFTSize int // FFT length (power of two); sets frequency resolution
	FFTBins int // bins actually transmitted; FFT output is max-pooled to this
	FFTRate int // FFT frames per second

	// Audio
	OpusBitrate int
	// OpusComplexity is the encoder's analysis effort, 0-10.
	//
	// This is the single most expensive stage in the pipeline and the only one
	// that switches on when someone talks, so it sets how much headroom a
	// transmission eats. Measured on one 20 ms frame of 16 kHz mono voice:
	// complexity 9 (libopus's default here) costs 119 µs, complexity 5 costs
	// 55 µs. That 64 µs is comparable to the entire wideband channel filter.
	//
	// The effort buys rate-distortion search depth, which pays off on wideband
	// music at high bitrates — not on 16 kHz voice at 16-32 kbps, where the
	// bitrate is the binding constraint and the extra searching finds almost
	// nothing. 5 is what realtime voice stacks generally settle on.
	OpusComplexity int
	// AudioSeq emits DatagramAudioSeq (sequence-numbered) instead of
	// DatagramAudio, which is what lets clients redeem the Opus in-band FEC the
	// encoder is already paying for. Clients predating sequence support ignore
	// the new type and go silent, so this flag exists as an escape hatch for
	// running an old client build against an updated Pi.
	AudioSeq bool

	// AMCarrierTrack follows the transmitting carrier within the AM channel and
	// filters narrowly around it. Aviation ground stations often sit several kHz
	// off the nominal channel (offset-carrier operation), so a narrow filter
	// fixed on centre would cut them off — tracking gives a narrow filter, and
	// therefore much less hiss, without losing them.
	AMCarrierTrack bool
	// AMHalfBandwidthHz is the narrow filter's half-bandwidth once centred.
	// Aviation AM voice occupies roughly ±3.5 kHz.
	AMHalfBandwidthHz float64
	// AMPresenceDb lifts the consonant band of AM voice; 0 disables.
	//
	// Airband AM is inherently muffled, and consonants are distinguished almost
	// entirely by energy between ~1.5 and 3 kHz, so a broad boost there is most
	// of what makes a communications receiver sound crisp rather than woolly.
	// It buys intelligibility, not signal-to-noise: noise in that band is
	// lifted by exactly as much as the voice. See dsp.PresenceEQ.
	AMPresenceDb float64

	// Diagnostics
	PProfAddr string // e.g. "localhost:6060"; empty disables
}

func DefaultConfig() *Config {
	return &Config{
		Host:     "0.0.0.0",
		Port:     4444,
		CertFile: "certs/server.crt",
		KeyFile:  "certs/server.key",
		// 960 kS/s, not 2.4 MS/s. Every upstream DSP cost scales with this —
		// sample conversion, FIR history pushes, and the channel filter's tap
		// count (53·fs/(22·BW), so 145 taps instead of 361) — making this alone
		// worth ~2.5× less CPU, which on a Pi in a hot enclosure is the
		// difference between steady state and thermal throttling. It must stay
		// an exact multiple of 48000 so every mode's audio rate divides it
		// (see Validate). 960 kHz of span is also already more than a phone
		// screen can usefully display.
		SampleRate:    960_000,
		DeviceIndex:   0,
		TunerGainAuto: true,
		FrequencyHz:   144_800_000,
		DemodMode:     protocol.ModeNFM,
		SquelchDBm:    -80,
		// Hysteresis stays small — it exists to stop noise flickering right at
		// the set point, nothing more. Wider values (6 dB+) surprise the user:
		// raising the squelch above a signal wouldn't mute it, because the
		// close point sat 6 dB lower still.
		//
		// Riding out the dips between syllables is the hang timer's job, and
		// 500 ms comfortably covers a pause in speech without leaving the
		// channel audibly open after a transmission ends.
		SquelchHysteresisDb: 3,
		SquelchHangMs:       500,
		FFTSize:             1024,
		FFTBins:             256,
		FFTRate:             10,
		OpusBitrate:         32000,
		OpusComplexity:      5,
		AudioSeq:            true,
		AMCarrierTrack:      true,
		AMHalfBandwidthHz:   3500,
		AMPresenceDb:        5,
	}
}

func (c *Config) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.Host, "host", c.Host, "Listen address")
	fs.IntVar(&c.Port, "port", c.Port, "Listen port (QUIC/UDP)")
	fs.StringVar(&c.CertFile, "cert", c.CertFile, "TLS certificate file")
	fs.StringVar(&c.KeyFile, "key", c.KeyFile, "TLS private key file")
	fs.BoolVar(&c.Simulate, "simulate", c.Simulate, "Use simulated SDR (no hardware needed)")
	fs.IntVar(&c.SampleRate, "samplerate", c.SampleRate, "RTL-SDR sample rate (must be a multiple of 48000)")
	fs.IntVar(&c.DeviceIndex, "device", c.DeviceIndex, "RTL-SDR device index")
	fs.StringVar(&c.DeviceSerial, "serial", c.DeviceSerial, "RTL-SDR device serial (overrides --device)")
	fs.IntVar(&c.FFTSize, "fftsize", c.FFTSize, "FFT length (power of two)")
	fs.IntVar(&c.FFTBins, "fftbins", c.FFTBins, "FFT bins transmitted per frame (max-pooled from fftsize)")
	fs.IntVar(&c.FFTRate, "fftrate", c.FFTRate, "FFT frames per second")
	fs.Float64Var(&c.SquelchHysteresisDb, "squelch-hysteresis", c.SquelchHysteresisDb,
		"dB below the squelch threshold before the gate closes")
	fs.Float64Var(&c.SquelchHangMs, "squelch-hang", c.SquelchHangMs,
		"ms to hold the squelch open after the signal drops")
	fs.IntVar(&c.OpusBitrate, "opus-bitrate", c.OpusBitrate, "Opus encoder bitrate (bps)")
	fs.IntVar(&c.OpusComplexity, "opus-complexity", c.OpusComplexity,
		"Opus encoder complexity 0-10; the pipeline's most expensive stage, and 5 is inaudible from 10 at voice bitrates")
	fs.BoolVar(&c.AMCarrierTrack, "am-carrier-track", c.AMCarrierTrack,
		"follow the transmitting carrier within an AM channel and filter narrowly around it")
	fs.Float64Var(&c.AMHalfBandwidthHz, "am-bandwidth", c.AMHalfBandwidthHz,
		"half-bandwidth in Hz of the narrow AM filter once centred on the carrier")
	fs.Float64Var(&c.AMPresenceDb, "am-presence", c.AMPresenceDb,
		"dB of presence lift on the AM consonant band (~2 kHz); 0 disables")
	fs.BoolVar(&c.AudioSeq, "audio-seq", c.AudioSeq, "Send sequence-numbered audio datagrams (needed for client-side Opus FEC)")
	fs.StringVar(&c.PProfAddr, "pprof", c.PProfAddr, "Serve net/http/pprof on this address (e.g. localhost:6060); empty disables")
}

// Validate rejects configurations that would silently misbehave at runtime.
//
// The load-bearing one is the sample-rate check. buildDSPChain derives its
// decimation as sampleRate/audioRate with integer division, so a rate that
// isn't an exact multiple yields audio clocked slightly wrong — 1.024 MS/s in
// WFM gives 21 instead of 21.33, i.e. audio 1.6 % off pitch and drifting
// against the Opus frame clock, with nothing logged anywhere. Mode changes at
// runtime, so every mode has to divide the rate, not just the starting one.
func (c *Config) Validate() error {
	if c.SampleRate <= 0 {
		return fmt.Errorf("samplerate must be positive, got %d", c.SampleRate)
	}
	for _, m := range []protocol.DemodMode{protocol.ModeNFM, protocol.ModeWFM, protocol.ModeAM} {
		if rate := m.AudioRate(); c.SampleRate%rate != 0 {
			return fmt.Errorf("samplerate %d is not an exact multiple of the %s audio rate (%d): "+
				"use a multiple of 48000, e.g. 960000, 1440000, 1920000 or 2400000",
				c.SampleRate, m, rate)
		}
		if bw := m.ChannelBandwidth(); c.SampleRate < 2*bw {
			return fmt.Errorf("samplerate %d is below Nyquist for %s (%d Hz channel needs at least %d)",
				c.SampleRate, m, bw, 2*bw)
		}
	}
	if c.FFTSize < 2 || c.FFTSize&(c.FFTSize-1) != 0 {
		return fmt.Errorf("fftsize must be a power of two of at least 2, got %d", c.FFTSize)
	}
	if c.FFTBins < 1 || c.FFTBins > c.FFTSize {
		return fmt.Errorf("fftbins must be between 1 and fftsize (%d), got %d", c.FFTSize, c.FFTBins)
	}
	if c.FFTRate < 1 {
		return fmt.Errorf("fftrate must be at least 1, got %d", c.FFTRate)
	}
	if c.OpusBitrate < 6000 {
		return fmt.Errorf("opus-bitrate %d is below the usable Opus minimum (6000)", c.OpusBitrate)
	}
	if c.AMPresenceDb < 0 || c.AMPresenceDb > 12 {
		return fmt.Errorf("am-presence must be between 0 and 12 dB, got %.1f", c.AMPresenceDb)
	}
	if c.OpusComplexity < 0 || c.OpusComplexity > 10 {
		return fmt.Errorf("opus-complexity must be between 0 and 10, got %d", c.OpusComplexity)
	}
	return nil
}
