// Package sdr wraps librtlsdr for RTL-SDR device access.
package sdr

import (
	"fmt"
	"log"
	"sync"

	rtl "github.com/ob3rg/gortlsdr"
)

// Device wraps a single RTL-SDR dongle.
type Device struct {
	dev *rtl.Context
	mu  sync.Mutex

	SampleRate int
	CenterFreq uint64
}

// OpenBySerial opens an RTL-SDR device matching the given serial string.
func OpenBySerial(serial string, sampleRate int, centerFreq uint64) (*Device, error) {
	index, err := rtl.GetIndexBySerial(serial)
	if err != nil {
		return nil, fmt.Errorf("rtlsdr lookup serial %q: %w", serial, err)
	}
	log.Printf("[sdr] serial %q resolved to device index %d", serial, index)
	return Open(index, sampleRate, centerFreq)
}

// Open opens RTL-SDR device at the given index.
func Open(index int, sampleRate int, centerFreq uint64) (*Device, error) {
	dev, err := rtl.Open(index)
	if err != nil {
		return nil, fmt.Errorf("rtlsdr open device %d: %w", index, err)
	}

	if err := dev.SetSampleRate(sampleRate); err != nil {
		dev.Close()
		return nil, fmt.Errorf("set sample rate %d: %w", sampleRate, err)
	}
	if err := dev.SetCenterFreq(int(centerFreq)); err != nil {
		dev.Close()
		return nil, fmt.Errorf("set center freq %d: %w", centerFreq, err)
	}
	if err := dev.SetTunerGainMode(false); err != nil {
		dev.Close()
		return nil, fmt.Errorf("set gain mode: %w", err)
	}
	if err := dev.ResetBuffer(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("reset buffer: %w", err)
	}

	log.Printf("[sdr] opened device %d: sample_rate=%d center_freq=%d",
		index, sampleRate, centerFreq)

	return &Device{
		dev:        dev,
		SampleRate: sampleRate,
		CenterFreq: centerFreq,
	}, nil
}

// Close releases the device.
func (d *Device) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dev.Close()
}

// SetCenterFreq tunes to the given frequency in Hz.
func (d *Device) SetCenterFreq(hz uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.dev.SetCenterFreq(int(hz)); err != nil {
		return err
	}
	d.CenterFreq = hz
	return nil
}

// SetGain sets the tuner gain in dB. If auto is true, gain is ignored.
func (d *Device) SetGain(auto bool, gainDB float64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if auto {
		return d.dev.SetTunerGainMode(false)
	}
	if err := d.dev.SetTunerGainMode(true); err != nil {
		return err
	}
	return d.dev.SetTunerGain(int(gainDB * 10)) // tenths of dB
}

// ReadAsync starts async reading. The callback receives raw CU8 IQ buffers.
// This blocks until CancelAsync is called.
func (d *Device) ReadAsync(cb func(buf []byte), bufCount, bufSize int) error {
	return d.dev.ReadAsync(rtl.ReadAsyncCbT(cb), nil, bufCount, bufSize)
}

// CancelAsync stops the async read loop.
func (d *Device) CancelAsync() error {
	return d.dev.CancelAsync()
}

func (d *Device) GetSampleRate() int    { return d.SampleRate }
func (d *Device) GetCenterFreq() uint64 { return d.CenterFreq }
