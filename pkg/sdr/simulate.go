package sdr

import (
	"log"
	"math"
	"math/rand"
	"sync"
	"time"
)

// SimulatedDevice generates synthetic IQ data that looks like real SDR output.
// Signals are at fixed absolute frequencies. When the center frequency changes,
// signals shift in the IQ baseband accordingly.
type SimulatedDevice struct {
	mu         sync.Mutex
	SampleRate int
	CenterFreq uint64
	cancelled  chan struct{}
	running    bool

	// Simulated signals at absolute frequencies
	signals []simSignal
}

type simSignal struct {
	freqHz    uint64  // absolute frequency in Hz
	amplitude float64 // 0-1
	freqDev   float64 // FM deviation in Hz
	audioHz   float64 // modulating audio tone frequency
}

// OpenSimulated creates a fake SDR device with signals at fixed absolute frequencies.
func OpenSimulated(sampleRate int, centerFreq uint64) *SimulatedDevice {
	log.Printf("[sdr-sim] opened simulated device: sample_rate=%d center_freq=%d", sampleRate, centerFreq)
	return &SimulatedDevice{
		SampleRate: sampleRate,
		CenterFreq: centerFreq,
		cancelled:  make(chan struct{}),
		signals: []simSignal{
			// VHF signals
			{freqHz: 144_800_000, amplitude: 0.4, freqDev: 3000, audioHz: 800},
			{freqHz: 144_900_000, amplitude: 0.25, freqDev: 5000, audioHz: 1200},
			{freqHz: 145_500_000, amplitude: 0.15, freqDev: 2000, audioHz: 600},
			{freqHz: 145_000_000, amplitude: 0.3, freqDev: 4000, audioHz: 1000},
			// UHF signals
			{freqHz: 433_500_000, amplitude: 0.35, freqDev: 3000, audioHz: 700},
			{freqHz: 446_006_250, amplitude: 0.2, freqDev: 2500, audioHz: 1500},
			// Airband (AM)
			{freqHz: 121_500_000, amplitude: 0.3, freqDev: 3000, audioHz: 400},
			{freqHz: 123_450_000, amplitude: 0.2, freqDev: 2000, audioHz: 900},
		},
	}
}

func (d *SimulatedDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running {
		close(d.cancelled)
		d.running = false
	}
	return nil
}

func (d *SimulatedDevice) SetCenterFreq(hz uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.CenterFreq = hz
	log.Printf("[sdr-sim] center freq → %d Hz", hz)
	return nil
}

func (d *SimulatedDevice) SetGain(auto bool, gainDB float64) error {
	log.Printf("[sdr-sim] gain: auto=%v db=%.1f", auto, gainDB)
	return nil
}

// ReadAsync generates synthetic CU8 IQ buffers and calls cb at ~real-time rate.
func (d *SimulatedDevice) ReadAsync(cb func(buf []byte), bufCount, bufSize int) error {
	d.mu.Lock()
	d.cancelled = make(chan struct{})
	d.running = true
	d.mu.Unlock()

	samplesPerBuf := bufSize / 2 // CU8: 2 bytes per sample
	interval := time.Duration(float64(time.Second) * float64(samplesPerBuf) / float64(d.SampleRate))

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	phase := make([]float64, len(d.signals))
	modPhase := make([]float64, len(d.signals))

	for {
		select {
		case <-d.cancelled:
			return nil
		case <-ticker.C:
			buf := make([]byte, bufSize)
			sr := float64(d.SampleRate)
			halfBW := sr / 2.0

			d.mu.Lock()
			centerFreq := float64(d.CenterFreq)
			signals := d.signals
			d.mu.Unlock()

			for i := 0; i < samplesPerBuf; i++ {
				var re, im float64

				for s := range signals {
					sig := &signals[s]
					// Compute offset from current center frequency
					offset := float64(sig.freqHz) - centerFreq

					// Only generate signal if within bandwidth
					if offset > halfBW || offset < -halfBW {
						continue
					}

					// FM-modulated tone
					modPhase[s] += 2 * math.Pi * sig.audioHz / sr
					if modPhase[s] > 2*math.Pi {
						modPhase[s] -= 2 * math.Pi
					}
					instFreq := offset + sig.freqDev*math.Sin(modPhase[s])
					phase[s] += 2 * math.Pi * instFreq / sr
					if phase[s] > 2*math.Pi {
						phase[s] -= 2 * math.Pi
					}

					re += sig.amplitude * math.Cos(phase[s])
					im += sig.amplitude * math.Sin(phase[s])
				}

				// Add noise
				noiseAmp := 0.05
				re += noiseAmp * (rand.Float64()*2 - 1)
				im += noiseAmp * (rand.Float64()*2 - 1)

				// Convert to CU8
				buf[2*i] = floatToCU8(re)
				buf[2*i+1] = floatToCU8(im)
			}

			cb(buf)
		}
	}
}

func (d *SimulatedDevice) CancelAsync() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running {
		close(d.cancelled)
		d.running = false
	}
	return nil
}

func (d *SimulatedDevice) GetSampleRate() int    { return d.SampleRate }
func (d *SimulatedDevice) GetCenterFreq() uint64 { return d.CenterFreq }

func floatToCU8(v float64) byte {
	scaled := v*128.0 + 127.5
	if scaled < 0 {
		return 0
	}
	if scaled > 255 {
		return 255
	}
	return byte(scaled)
}
