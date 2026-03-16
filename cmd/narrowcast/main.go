package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/pierr3/narrowcast/pkg/audio"
	"github.com/pierr3/narrowcast/pkg/config"
	"github.com/pierr3/narrowcast/pkg/dsp"
	"github.com/pierr3/narrowcast/pkg/protocol"
	"github.com/pierr3/narrowcast/pkg/sdr"

	"github.com/quic-go/quic-go"
)

func main() {
	cfg := config.DefaultConfig()
	cfg.RegisterFlags(flag.CommandLine)
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run(ctx context.Context, cfg *config.Config) error {
	// Open SDR (real or simulated)
	var dev sdr.SDRDevice
	if cfg.Simulate {
		dev = sdr.OpenSimulated(cfg.SampleRate, cfg.FrequencyHz)
		log.Printf("[narrowcast] using SIMULATED SDR device")
	} else if cfg.DeviceSerial != "" {
		realDev, err := sdr.OpenBySerial(cfg.DeviceSerial, cfg.SampleRate, cfg.FrequencyHz)
		if err != nil {
			return fmt.Errorf("sdr: %w", err)
		}
		dev = realDev
	} else {
		realDev, err := sdr.Open(cfg.DeviceIndex, cfg.SampleRate, cfg.FrequencyHz)
		if err != nil {
			return fmt.Errorf("sdr: %w", err)
		}
		dev = realDev
	}
	defer dev.Close()

	// Shared state protected by mutex
	state := &serverState{
		cfg:       cfg,
		dev:       dev,
		mode:      cfg.DemodMode,
		freqHz:    cfg.FrequencyHz,
		squelchDb: cfg.SquelchDBm,
	}

	// IQ sample fan-out: SDR callback pushes to a channel, pipeline goroutine consumes
	iqChan := make(chan []byte, 16)

	// Start SDR async read in a goroutine
	go func() {
		bufSize := cfg.SampleRate / 10 * 2 // ~100ms of CU8 data
		err := dev.ReadAsync(func(buf []byte) {
			// Copy buffer since the SDR reuses it
			cp := make([]byte, len(buf))
			copy(cp, buf)
			select {
			case iqChan <- cp:
			default:
				// Drop if pipeline can't keep up
			}
		}, 12, bufSize)
		if err != nil {
			log.Printf("[sdr] read async error: %v", err)
		}
	}()
	defer dev.CancelAsync()

	// Start QUIC server
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	srv, err := protocol.NewServer(addr, cfg.CertFile, cfg.KeyFile,
		func(clientCtx context.Context, conn quic.Connection) {
			handleClient(clientCtx, conn, state, iqChan)
		})
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}
	defer srv.Close()

	log.Printf("[narrowcast] server starting on %s (freq=%d Hz, mode=%s)",
		addr, cfg.FrequencyHz, cfg.DemodMode)

	return srv.Serve(ctx)
}

// serverState holds mutable SDR state shared between clients and the pipeline.
type serverState struct {
	cfg       *config.Config
	dev       sdr.SDRDevice
	mu        sync.RWMutex
	mode      protocol.DemodMode
	freqHz    uint64
	squelchDb float32
}

func handleClient(ctx context.Context, conn quic.Connection, state *serverState, iqChan <-chan []byte) {
	remote := conn.RemoteAddr().String()

	// --- Command loop + streaming ---
	// All commands are sent as datagrams: [uint8 type][payload...]
	// (Same format as data datagrams, but with command type bytes 0x10-0x31)
	streaming := false
	stopStreaming := make(chan struct{})
	modeChan := make(chan protocol.DemodMode, 1)
	var streamWg sync.WaitGroup

	defer func() {
		if streaming {
			close(stopStreaming)
			streamWg.Wait()
		}
	}()

	// Send Welcome as a datagram immediately
	state.mu.RLock()
	welcome := []byte{protocol.CmdWelcome, protocol.ProtoVersion}
	welcome = append(welcome, protocol.EncodeUint64(24_000_000)...)    // min freq
	welcome = append(welcome, protocol.EncodeUint64(1_766_000_000)...) // max freq
	welcome = append(welcome, protocol.EncodeFloat32(float32(state.cfg.SampleRate))...)
	state.mu.RUnlock()
	if err := conn.SendDatagram(welcome); err != nil {
		log.Printf("[client %s] welcome send: %v", remote, err)
		return
	}
	log.Printf("[client %s] sent Welcome datagram", remote)

	for {
		// Receive command datagrams
		dgram, err := conn.ReceiveDatagram(ctx)
		if err != nil {
			log.Printf("[client %s] recv datagram: %v", remote, err)
			return
		}
		if len(dgram) < 1 {
			continue
		}

		cmdType := dgram[0]
		payload := dgram[1:]

		switch cmdType {
		case protocol.CmdHello:
			if len(payload) >= 1 {
				log.Printf("[client %s] Hello v%d", remote, payload[0])
			}

		case protocol.CmdSetFrequency:
			if len(payload) < 8 {
				continue
			}
			hz := protocol.DecodeUint64(payload)
			state.mu.Lock()
			state.freqHz = hz
			state.mu.Unlock()
			if err := state.dev.SetCenterFreq(hz); err != nil {
				log.Printf("[client %s] set freq: %v", remote, err)
			}
			log.Printf("[client %s] freq → %d Hz", remote, hz)

		case protocol.CmdSetMode:
			if len(payload) < 1 {
				continue
			}
			mode := protocol.DemodMode(payload[0])
			state.mu.Lock()
			state.mode = mode
			state.mu.Unlock()
			select {
			case modeChan <- mode:
			default:
				select {
				case <-modeChan:
				default:
				}
				modeChan <- mode
			}
			log.Printf("[client %s] mode → %s", remote, mode)

		case protocol.CmdSetSquelch:
			if len(payload) < 4 {
				continue
			}
			db := protocol.DecodeFloat32(payload)
			state.mu.Lock()
			state.squelchDb = db
			state.mu.Unlock()
			log.Printf("[client %s] squelch → %.1f dBm", remote, db)

		case protocol.CmdSetGain:
			if len(payload) < 4 {
				continue
			}
			gain := protocol.DecodeFloat32(payload)
			auto := gain == 0
			if err := state.dev.SetGain(auto, float64(gain)); err != nil {
				log.Printf("[client %s] set gain: %v", remote, err)
			}

		case protocol.CmdStart:
			if streaming {
				continue
			}
			streaming = true
			stopStreaming = make(chan struct{})
			streamWg.Add(1)
			go func() {
				defer streamWg.Done()
				runPipeline(ctx, conn, state, iqChan, stopStreaming, modeChan)
			}()
			log.Printf("[client %s] streaming started", remote)

		case protocol.CmdStop:
			if streaming {
				close(stopStreaming)
				streamWg.Wait()
				streaming = false
				log.Printf("[client %s] streaming stopped", remote)
			}
		}
	}
}

// buildDSPChain constructs the DSP objects for a given demod mode.
type dspChain struct {
	xlat       *dsp.XlatingFilter
	demodFn    func([]complex128) []float64
	deemph     *dsp.DeEmphasis
	audioDecim int
	opusEnc    *audio.OpusEncoder
	audioRate  int
}

func buildDSPChain(mode protocol.DemodMode, sampleRate int, opusBitrate int) (*dspChain, error) {
	channelBW := mode.ChannelBandwidth()
	audioRate := mode.AudioRate()

	// Total decimation must be exact: sampleRate / totalDecim = audioRate
	totalDecim := sampleRate / audioRate

	// Minimum xlating decimation to satisfy Nyquist for the channel bandwidth
	minXlatDecim := sampleRate / (channelBW * 2)
	if minXlatDecim < 1 {
		minXlatDecim = 1
	}

	// Find smallest divisor of totalDecim >= minXlatDecim
	xlatDecim := minXlatDecim
	for totalDecim%xlatDecim != 0 {
		xlatDecim++
	}
	audioDecim := totalDecim / xlatDecim
	if audioDecim < 1 {
		audioDecim = 1
	}

	decimatedRate := float64(sampleRate) / float64(xlatDecim)

	numTaps := 53*sampleRate/(22*channelBW) | 1
	if numTaps > 511 {
		numTaps = 511
	}
	lpfTaps := dsp.NewLowPassFIR(float64(channelBW)/2, float64(sampleRate), numTaps)
	xlat := dsp.NewXlatingFilter(0, lpfTaps, xlatDecim, float64(sampleRate))

	log.Printf("[dsp] mode=%s xlatDecim=%d audioDecim=%d decimatedRate=%.0f audioRate=%d",
		mode, xlatDecim, audioDecim, decimatedRate, audioRate)

	var demodFn func([]complex128) []float64
	var deemph *dsp.DeEmphasis

	switch mode {
	case protocol.ModeNFM:
		fmDemod := dsp.NewFMDemodulator(5000, decimatedRate)
		deemph = dsp.NewDeEmphasis(50e-6, float64(audioRate))
		demodFn = fmDemod.Demodulate
	case protocol.ModeWFM:
		fmDemod := dsp.NewFMDemodulator(75000, decimatedRate)
		deemph = dsp.NewDeEmphasis(50e-6, float64(audioRate))
		demodFn = fmDemod.Demodulate
	case protocol.ModeAM:
		amDemod := dsp.NewAMDemodulator()
		demodFn = amDemod.Demodulate
	}

	opusEnc, err := audio.NewOpusEncoder(audioRate, opusBitrate)
	if err != nil {
		return nil, err
	}

	return &dspChain{
		xlat:       xlat,
		demodFn:    demodFn,
		deemph:     deemph,
		audioDecim: audioDecim,
		opusEnc:    opusEnc,
		audioRate:  audioRate,
	}, nil
}

// runPipeline reads IQ data, demodulates, encodes, and sends datagrams.
func runPipeline(ctx context.Context, conn quic.Connection, state *serverState, iqChan <-chan []byte, stop <-chan struct{}, modeChan <-chan protocol.DemodMode) {
	state.mu.RLock()
	mode := state.mode
	sampleRate := state.cfg.SampleRate
	fftSize := state.cfg.FFTSize
	fftInterval := time.Second / time.Duration(state.cfg.FFTRate)
	opusBitrate := state.cfg.OpusBitrate
	state.mu.RUnlock()

	chain, err := buildDSPChain(mode, sampleRate, opusBitrate)
	if err != nil {
		log.Printf("[pipeline] dsp chain error: %v", err)
		return
	}

	// FFT state
	fftBuf := make([]complex128, 0, fftSize)
	lastFFT := time.Now()

	// Status state (send ~4 times per second)
	lastStatus := time.Now()
	statusInterval := 250 * time.Millisecond
	var signalPowerDb float32 = -120

	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case newMode := <-modeChan:
			if newMode == mode {
				continue
			}
			newChain, err := buildDSPChain(newMode, sampleRate, opusBitrate)
			if err != nil {
				log.Printf("[pipeline] rebuild dsp chain: %v", err)
				continue
			}
			chain = newChain
			mode = newMode
			fftBuf = fftBuf[:0]
			log.Printf("[pipeline] DSP chain rebuilt for mode %s", mode)

		case rawBuf, ok := <-iqChan:
			if !ok {
				return
			}

			// Convert CU8 → complex
			iq := dsp.CU8ToComplex(rawBuf)

			// --- FFT waterfall (on raw wideband IQ) ---
			fftBuf = append(fftBuf, iq...)
			if len(fftBuf) >= fftSize && time.Since(lastFFT) >= fftInterval {
				fftFrame := make([]complex128, fftSize)
				copy(fftFrame, fftBuf[:fftSize])
				fftBuf = fftBuf[fftSize:]
				lastFFT = time.Now()

				dsp.HannWindow(fftFrame)
				dsp.FFT(fftFrame)
				bins := dsp.MagnitudeToU8(fftFrame)

				// Send FFT datagram: [type][uint16 numBins][bins...]
				dgram := make([]byte, 3+len(bins))
				dgram[0] = protocol.DatagramFFT
				dgram[1] = byte(len(bins) >> 8)
				dgram[2] = byte(len(bins))
				copy(dgram[3:], bins)
				_ = conn.SendDatagram(dgram)
			}
			// Keep FFT buffer bounded
			if len(fftBuf) > fftSize*4 {
				fftBuf = fftBuf[len(fftBuf)-fftSize:]
			}

			// --- Channel filter + demodulate ---
			channelIQ := chain.xlat.Process(iq)
			if len(channelIQ) == 0 {
				continue
			}

			audioSamples := chain.demodFn(channelIQ)

			// Audio decimation to target rate
			if chain.audioDecim > 1 {
				decimated := make([]float64, 0, len(audioSamples)/chain.audioDecim)
				for i := 0; i < len(audioSamples); i += chain.audioDecim {
					decimated = append(decimated, audioSamples[i])
				}
				audioSamples = decimated
			}

			// De-emphasis for FM modes
			if chain.deemph != nil {
				chain.deemph.Process(audioSamples)
			}

			// Measure signal power for S-meter (before normalization)
			var sumSq float64
			for _, s := range audioSamples {
				sumSq += s * s
			}
			if len(audioSamples) > 0 {
				rms := math.Sqrt(sumSq / float64(len(audioSamples)))
				if rms > 1e-10 {
					signalPowerDb = float32(20 * math.Log10(rms))
				} else {
					signalPowerDb = -120
				}
			}

			// Send status datagram periodically
			if time.Since(lastStatus) >= statusInterval {
				lastStatus = time.Now()
				state.mu.RLock()
				sq := state.squelchDb
				m := state.mode
				state.mu.RUnlock()
				statusDgram := make([]byte, 10)
				statusDgram[0] = protocol.DatagramStatus
				copy(statusDgram[1:5], protocol.EncodeFloat32(signalPowerDb))
				copy(statusDgram[5:9], protocol.EncodeFloat32(sq))
				statusDgram[9] = byte(m)
				_ = conn.SendDatagram(statusDgram)
			}

			// Squelch gate: skip audio when signal is below threshold
			state.mu.RLock()
			squelchThreshold := state.squelchDb
			state.mu.RUnlock()
			if signalPowerDb < squelchThreshold {
				continue
			}

			// Normalize audio level
			var maxAbs float64
			for _, s := range audioSamples {
				if a := math.Abs(s); a > maxAbs {
					maxAbs = a
				}
			}
			if maxAbs > 0.001 {
				scale := 0.8 / maxAbs
				if scale > 10 {
					scale = 10 // limit AGC gain
				}
				for i := range audioSamples {
					audioSamples[i] *= scale
				}
			}

			// Opus encode
			packets, err := chain.opusEnc.Encode(audioSamples)
			if err != nil {
				log.Printf("[pipeline] opus: %v", err)
				continue
			}

			// Send audio datagrams
			for _, pkt := range packets {
				dgram := make([]byte, 1+len(pkt))
				dgram[0] = protocol.DatagramAudio
				copy(dgram[1:], pkt)
				_ = conn.SendDatagram(dgram)
			}
		}
	}
}
