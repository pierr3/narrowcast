package config

import (
	"flag"

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
	TunerGainAuto bool
	TunerGain     float64

	// Defaults
	FrequencyHz uint64
	DemodMode   protocol.DemodMode
	SquelchDBm  float32

	// DSP
	FFTSize int
	FFTRate int // FFT frames per second

	// Audio
	OpusBitrate int
}

func DefaultConfig() *Config {
	return &Config{
		Host:          "0.0.0.0",
		Port:          4444,
		CertFile:      "certs/server.crt",
		KeyFile:       "certs/server.key",
		SampleRate:    2_400_000,
		DeviceIndex:   0,
		TunerGainAuto: true,
		FrequencyHz:   144_800_000,
		DemodMode:     protocol.ModeNFM,
		SquelchDBm:    -80,
		FFTSize:       1024,
		FFTRate:       10,
		OpusBitrate:   32000,
	}
}

func (c *Config) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.Host, "host", c.Host, "Listen address")
	fs.IntVar(&c.Port, "port", c.Port, "Listen port (QUIC/UDP)")
	fs.StringVar(&c.CertFile, "cert", c.CertFile, "TLS certificate file")
	fs.StringVar(&c.KeyFile, "key", c.KeyFile, "TLS private key file")
	fs.BoolVar(&c.Simulate, "simulate", c.Simulate, "Use simulated SDR (no hardware needed)")
	fs.IntVar(&c.SampleRate, "samplerate", c.SampleRate, "RTL-SDR sample rate")
	fs.IntVar(&c.DeviceIndex, "device", c.DeviceIndex, "RTL-SDR device index")
	fs.IntVar(&c.FFTSize, "fftsize", c.FFTSize, "FFT bin count")
	fs.IntVar(&c.FFTRate, "fftrate", c.FFTRate, "FFT frames per second")
	fs.IntVar(&c.OpusBitrate, "opus-bitrate", c.OpusBitrate, "Opus encoder bitrate (bps)")
}
