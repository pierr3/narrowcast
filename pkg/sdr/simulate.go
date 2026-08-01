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

// modulation of a simulated signal.
type modulation int

const (
	modFM modulation = iota
	modAM
)

type simSignal struct {
	freqHz    uint64     // nominal channel frequency in Hz
	amplitude float64    // 0-1
	mod       modulation // FM or DSB-AM
	freqDev   float64    // FM deviation in Hz (FM only)
	audioHz   float64    // modulating audio tone frequency
	// carrierOffsetHz shifts the carrier away from the nominal channel, which
	// is what aviation ground stations do in offset-carrier ("Climax")
	// operation: several transmitters cover one sector on staggered carriers,
	// while aircraft transmit on the nominal frequency.
	carrierOffsetHz float64
	// amDepth is the AM modulation index (0-1).
	amDepth float64
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
			{freqHz: 144_800_000, amplitude: 0.4, mod: modFM, freqDev: 3000, audioHz: 800},
			{freqHz: 144_900_000, amplitude: 0.25, mod: modFM, freqDev: 5000, audioHz: 1200},
			{freqHz: 145_500_000, amplitude: 0.15, mod: modFM, freqDev: 2000, audioHz: 600},
			{freqHz: 145_000_000, amplitude: 0.3, mod: modFM, freqDev: 4000, audioHz: 1000},
			// UHF signals
			{freqHz: 433_500_000, amplitude: 0.35, mod: modFM, freqDev: 3000, audioHz: 700},
			{freqHz: 446_006_250, amplitude: 0.2, mod: modFM, freqDev: 2500, audioHz: 1500},

			// Airband, genuinely AM — these used to be FM like everything else,
			// which meant the AM demod path and anything reading an AM carrier
			// could never be exercised without real hardware.
			//
			// 121.500 is an on-channel aircraft-style transmission.
			{freqHz: 121_500_000, amplitude: 0.3, mod: modAM, audioHz: 400, amDepth: 0.7},
			{freqHz: 123_450_000, amplitude: 0.2, mod: modAM, audioHz: 900, amDepth: 0.6},
			// An 8.33 kHz neighbour of 121.500, carrying a distinct tone. In
			// Europe channels really are this close, and a channel filter wide
			// enough to catch offset carriers is also wide enough to let the
			// neighbour through — which is what makes narrowing worthwhile.
			{freqHz: 121_508_333, amplitude: 0.22, mod: modAM, audioHz: 1150, amDepth: 0.7},
			// 125.500 models a 25 kHz channel served by offset-carrier ground
			// transmitters: two carriers either side of nominal, no on-channel
			// one, exactly the case where a narrow filter fixed on centre hears
			// nothing at all.
			{freqHz: 125_500_000, amplitude: 0.25, mod: modAM, audioHz: 600, amDepth: 0.7, carrierOffsetHz: -7500},
			{freqHz: 125_500_000, amplitude: 0.18, mod: modAM, audioHz: 600, amDepth: 0.7, carrierOffsetHz: +7500},
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
					// Offset from the current centre, including any deliberate
					// carrier offset for this transmitter.
					offset := float64(sig.freqHz) + sig.carrierOffsetHz - centerFreq

					// Only generate signal if within bandwidth
					if offset > halfBW || offset < -halfBW {
						continue
					}

					modPhase[s] += 2 * math.Pi * sig.audioHz / sr
					if modPhase[s] > 2*math.Pi {
						modPhase[s] -= 2 * math.Pi
					}

					switch sig.mod {
					case modAM:
						// DSB-AM: constant-frequency carrier, amplitude varying
						// with the audio. The carrier stays put while the
						// envelope moves, which is the property AM squelch and
						// carrier tracking both rely on.
						phase[s] += 2 * math.Pi * offset / sr
						if phase[s] > 2*math.Pi {
							phase[s] -= 2 * math.Pi
						}
						env := sig.amplitude * (1 + sig.amDepth*math.Sin(modPhase[s]))
						re += env * math.Cos(phase[s])
						im += env * math.Sin(phase[s])
					default:
						// FM-modulated tone
						instFreq := offset + sig.freqDev*math.Sin(modPhase[s])
						phase[s] += 2 * math.Pi * instFreq / sr
						if phase[s] > 2*math.Pi {
							phase[s] -= 2 * math.Pi
						}
						re += sig.amplitude * math.Cos(phase[s])
						im += sig.amplitude * math.Sin(phase[s])
					}
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
