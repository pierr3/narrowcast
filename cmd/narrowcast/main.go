package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math"
	"os/signal"
	"sync"
	"sync/atomic"
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

	// Drop counter: SDR callback increments when iqChan is full. The pipeline
	// observes increases and resets DSP state — continuing to filter with
	// stale FIR/IIR history after a buffer gap produces audible warbling.
	state.dropCount = &atomic.Uint64{}

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
				// Pipeline can't keep up — drop and signal so it resets state.
				state.dropCount.Add(1)
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

	// dropCount is incremented by the SDR callback whenever iqChan is full.
	// The pipeline tracks the last observed value and resets DSP state when
	// it increases, so we don't keep filtering across a discontinuity.
	dropCount *atomic.Uint64
}

func handleClient(ctx context.Context, conn quic.Connection, state *serverState, iqChan <-chan []byte) {
	remote := conn.RemoteAddr().String()

	// --- Command loop + streaming ---
	// All commands are sent as datagrams: [uint8 type][payload...]
	// (Same format as data datagrams, but with command type bytes 0x10-0x31)
	streaming := false
	stopStreaming := make(chan struct{})
	modeChan := make(chan protocol.DemodMode, 1)
	// flushChan signals the pipeline to drain stale IQ buffers and reset DSP
	// state — used after a hardware retune so the listener doesn't hear a
	// transient pop while old samples drain through the filters.
	flushChan := make(chan struct{}, 1)
	// qualityChan delivers client-reported loss measurements (0-100 %) so the
	// pipeline can throttle FFT rate and reduce Opus bitrate. Not blocking
	// audio path: best-effort drop on full.
	qualityChan := make(chan qualityReport, 4)
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
			// Resend Welcome on every Hello (needed for relay: the initial
			// Welcome may have been sent before any client was connected)
			if err := conn.SendDatagram(welcome); err != nil {
				log.Printf("[client %s] welcome resend: %v", remote, err)
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
			// Tell the pipeline to drain stale IQ and reset DSP state so the
			// retune is heard as a clean cut, not a swept artifact.
			select {
			case flushChan <- struct{}{}:
			default:
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

		case protocol.CmdQualityReport:
			// Client reports measured packet loss vs the server's seq-marks.
			// Drives FFT throttle and Opus bitrate adaptation.
			// Payload: [u8 audioLoss][u8 fftLoss][u16 windowMs]
			if len(payload) < 4 {
				continue
			}
			qr := qualityReport{
				audioLossPct: payload[0],
				fftLossPct:   payload[1],
			}
			select {
			case qualityChan <- qr:
			default:
				// Drop oldest, push newest — staleness is worse than gap.
				select {
				case <-qualityChan:
				default:
				}
				qualityChan <- qr
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
				runPipeline(ctx, conn, state, iqChan, stopStreaming, modeChan, flushChan, qualityChan)
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

// gainStage is satisfied by any AGC variant — both pkg/dsp.AGC (FM) and
// pkg/dsp.AudioAGC (AM hang-time) implement Process and Reset.
type gainStage interface {
	Process([]float64)
	Reset()
}

// qualityReport carries the client's most recent loss measurement (0-100).
// audioLossPct drives Opus bitrate + FEC adaptation.
// fftLossPct drives FFT frame-rate throttling.
type qualityReport struct {
	audioLossPct byte
	fftLossPct   byte
}

// adaptFFTInterval picks an FFT send interval based on measured loss.
// Audio is the priority — at high loss we cut FFT bandwidth aggressively.
//
//	 < 2 %  →  full (configured rate, default 10 fps)
//	 2-10 % →  half (5 fps default)
//	10-25 % →  1/5  (2 fps default)
//	 >25 %  →  1/10 (1 fps default)
func adaptFFTInterval(base time.Duration, lossPct byte) time.Duration {
	switch {
	case lossPct < 2:
		return base
	case lossPct < 10:
		return base * 2
	case lossPct < 25:
		return base * 5
	default:
		return base * 10
	}
}

// adaptOpusBitrate picks an Opus bitrate based on measured loss.
// Floors at 16 kbps — below that, Opus voice quality drops below acceptable
// for the monitoring use case. We'd rather cut out cleanly than ship muddy
// audio. The clean cutout is QUIC's default behavior when datagrams stop
// flowing, so no extra logic needed at the floor.
//
//	 < 2 %  →  configured (default 32 kbps)
//	 2-5 % →  24 kbps
//	 >5 %  →  16 kbps  (floor)
func adaptOpusBitrate(base int, lossPct byte) int {
	const floor = 16000
	if base < floor {
		base = floor
	}
	switch {
	case lossPct < 2:
		return base
	case lossPct < 5:
		if base > 24000 {
			return 24000
		}
		return base
	default:
		return floor
	}
}

// adaptOpusLossPerc maps measured loss to the SetPacketLossPerc value the
// encoder uses to allocate FEC redundancy bits. Slightly above measured
// loss so FEC has headroom for short bursts.
func adaptOpusLossPerc(lossPct byte) int {
	v := int(lossPct) + 5
	if v < 5 {
		v = 5
	}
	if v > 50 {
		v = 50
	}
	return v
}

// buildDSPChain constructs the DSP objects for a given demod mode.
type dspChain struct {
	xlat        *dsp.XlatingFilter
	fmDemod     *dsp.FMDemodulator // non-nil for NFM/WFM
	amDemod     *dsp.AMDemodulator // non-nil for AM
	demodFn     func([]complex128) []float64
	deemph      *dsp.DeEmphasis
	audioDecimF *dsp.RealFIRFilter // anti-aliased decimation filter (nil if no decimation needed)
	voiceHPF    *dsp.HighPassIIR   // voice bandpass high-pass (AM only)
	voiceLPF    *dsp.RealFIRFilter // voice bandpass low-pass (AM only)
	limiter     *dsp.SoftLimiter   // soft clipper for ADC saturation
	gain        gainStage          // AGC (FM) or hang-time AudioAGC (AM)
	opusEnc     *audio.OpusEncoder
	audioRate   int
}

// Reset clears every stateful stage in the chain. Called after a hardware
// retune (drains transient samples from filter histories) and after the SDR
// callback has dropped buffers (continuity is broken so old history is
// noise, not signal).
func (c *dspChain) Reset() {
	if c.xlat != nil {
		c.xlat.Reset()
	}
	if c.fmDemod != nil {
		c.fmDemod.Reset()
	}
	if c.amDemod != nil {
		c.amDemod.Reset()
	}
	if c.deemph != nil {
		c.deemph.Reset()
	}
	if c.audioDecimF != nil {
		c.audioDecimF.Reset()
	}
	if c.voiceHPF != nil {
		c.voiceHPF.Reset()
	}
	if c.voiceLPF != nil {
		c.voiceLPF.Reset()
	}
	if c.gain != nil {
		c.gain.Reset()
	}
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

	// Build anti-aliased audio decimation filter
	var audioDecimF *dsp.RealFIRFilter
	if audioDecim > 1 {
		// Low-pass at audioRate/2 before decimation, operating at decimatedRate
		aaNumTaps := audioDecim*10 + 1 // ~10 taps per decimation factor
		if aaNumTaps > 127 {
			aaNumTaps = 127
		}
		aaTaps := dsp.NewLowPassFIR(float64(audioRate)/2, decimatedRate, aaNumTaps)
		audioDecimF = dsp.NewRealFIRDecimator(aaTaps, audioDecim)
	}

	log.Printf("[dsp] mode=%s xlatDecim=%d audioDecim=%d decimatedRate=%.0f audioRate=%d",
		mode, xlatDecim, audioDecim, decimatedRate, audioRate)

	var demodFn func([]complex128) []float64
	var deemph *dsp.DeEmphasis
	var fmDemod *dsp.FMDemodulator
	var amDemod *dsp.AMDemodulator

	switch mode {
	case protocol.ModeNFM:
		fmDemod = dsp.NewFMDemodulator(5000, decimatedRate)
		deemph = dsp.NewDeEmphasis(50e-6, float64(audioRate))
		demodFn = fmDemod.Demodulate
	case protocol.ModeWFM:
		fmDemod = dsp.NewFMDemodulator(75000, decimatedRate)
		deemph = dsp.NewDeEmphasis(50e-6, float64(audioRate))
		demodFn = fmDemod.Demodulate
	case protocol.ModeAM:
		amDemod = dsp.NewAMDemodulator()
		demodFn = amDemod.Demodulate
	}

	// AM voice cleanup: bandpass 400-3000 Hz
	// No noise gate for AM — squelch handles muting between transmissions,
	// and the gate causes crackle by chattering on AGC-amplified noise.
	var voiceHPF *dsp.HighPassIIR
	var voiceLPF *dsp.RealFIRFilter
	if mode == protocol.ModeAM {
		// 2nd-order high-pass at 400 Hz to kill carrier hum and rumble
		voiceHPF = dsp.NewHighPassIIR(400, float64(audioRate))
		// Low-pass at 3000 Hz to remove high-frequency noise
		lpfNumTaps := 65
		lpfTaps := dsp.NewLowPassFIR(3000, float64(audioRate), lpfNumTaps)
		voiceLPF = dsp.NewRealFIRDecimator(lpfTaps, 1) // decim=1, just filtering
		log.Printf("[dsp] AM voice cleanup: bandpass 400-3000 Hz")
	}

	// Soft limiter to tame ADC-saturated signals (drive=2.0 = moderate compression)
	// Skip for AM — amplitude IS the audio, so tanh compression distorts the voice.
	var limiter *dsp.SoftLimiter
	if mode != protocol.ModeAM {
		limiter = dsp.NewSoftLimiter(2.0)
	}

	// AM: hang-time AudioAGC. Standard AGC ramps gain into the noise floor
	// between transmissions and clips the start of the next one while attack
	// catches up. Hang-time AGC freezes gain during dead air so the next
	// transmission starts at the correct level instantly.
	// FM: regular AGC — amplitude doesn't carry info, fast attack is fine.
	var gain gainStage
	if mode == protocol.ModeAM {
		gain = dsp.NewAudioAGC(0.17, 0.1, 0.001, 500.0, float64(audioRate), 0.02)
		log.Printf("[dsp] AM hang-time AudioAGC: hang=500ms minMag=0.02")
	} else {
		gain = dsp.NewAGC(-12, 30, 20.0, 500.0, float64(audioRate))
	}

	opusEnc, err := audio.NewOpusEncoder(audioRate, opusBitrate)
	if err != nil {
		return nil, err
	}

	return &dspChain{
		xlat:        xlat,
		fmDemod:     fmDemod,
		amDemod:     amDemod,
		demodFn:     demodFn,
		deemph:      deemph,
		audioDecimF: audioDecimF,
		voiceHPF:    voiceHPF,
		voiceLPF:    voiceLPF,
		limiter:     limiter,
		gain:        gain,
		opusEnc:     opusEnc,
		audioRate:   audioRate,
	}, nil
}

// runPipeline reads IQ data, demodulates, encodes, and sends datagrams.
func runPipeline(ctx context.Context, conn quic.Connection, state *serverState, iqChan <-chan []byte, stop <-chan struct{}, modeChan <-chan protocol.DemodMode, flushChan <-chan struct{}, qualityChan <-chan qualityReport) {
	state.mu.RLock()
	mode := state.mode
	sampleRate := state.cfg.SampleRate
	fftSize := state.cfg.FFTSize
	baseFFTInterval := time.Second / time.Duration(state.cfg.FFTRate)
	baseOpusBitrate := state.cfg.OpusBitrate
	state.mu.RUnlock()

	chain, err := buildDSPChain(mode, sampleRate, baseOpusBitrate)
	if err != nil {
		log.Printf("[pipeline] dsp chain error: %v", err)
		return
	}

	// Track SDR drop count to detect IQ-buffer gaps. On increase we reset all
	// DSP state and skip the next block to avoid filtering across a
	// discontinuity (which sounds like warbling artifacts).
	lastDrops := state.dropCount.Load()

	// Adaptive state. fftInterval and currentBitrate start at configured
	// defaults and step down only when QualityReport indicates loss.
	fftInterval := baseFFTInterval
	currentBitrate := baseOpusBitrate

	// Sequence counters — included in DatagramSeqMark every second. The client
	// diffs these against its own receive counts to compute loss.
	var audioSent, fftSent, statusSent uint32
	const seqMarkInterval = 1 * time.Second
	lastSeqMark := time.Now()

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
			newChain, err := buildDSPChain(newMode, sampleRate, baseOpusBitrate)
			if err != nil {
				log.Printf("[pipeline] rebuild dsp chain: %v", err)
				continue
			}
			chain = newChain
			mode = newMode
			fftBuf = fftBuf[:0]
			// Re-apply current adaptive Opus settings to the freshly built encoder.
			_ = chain.opusEnc.SetBitrate(currentBitrate)
			log.Printf("[pipeline] DSP chain rebuilt for mode %s", mode)

		case qr := <-qualityChan:
			// Client reported its loss measurement. Adapt FFT rate and Opus
			// bitrate so the audio path stays viable on a struggling network.
			newFFTInterval := adaptFFTInterval(baseFFTInterval, qr.fftLossPct)
			newBitrate := adaptOpusBitrate(baseOpusBitrate, qr.audioLossPct)
			lossPerc := adaptOpusLossPerc(qr.audioLossPct)

			if newFFTInterval != fftInterval {
				log.Printf("[adapt] fft loss=%d%% interval %v→%v",
					qr.fftLossPct, fftInterval, newFFTInterval)
				fftInterval = newFFTInterval
			}
			if newBitrate != currentBitrate {
				if err := chain.opusEnc.SetBitrate(newBitrate); err != nil {
					log.Printf("[adapt] SetBitrate %d: %v", newBitrate, err)
				} else {
					log.Printf("[adapt] audio loss=%d%% bitrate %d→%d bps",
						qr.audioLossPct, currentBitrate, newBitrate)
					currentBitrate = newBitrate
				}
			}
			if err := chain.opusEnc.SetPacketLossPerc(lossPerc); err != nil {
				log.Printf("[adapt] SetPacketLossPerc %d: %v", lossPerc, err)
			}

		case <-flushChan:
			// Hardware was retuned. Drop any IQ buffered before the retune
			// (they're at the OLD frequency) and reset every stateful DSP
			// stage so post-retune audio doesn't carry pre-retune transient.
			drained := 0
		drainLoop:
			for {
				select {
				case <-iqChan:
					drained++
				default:
					break drainLoop
				}
			}
			chain.Reset()
			fftBuf = fftBuf[:0]
			// Sync the drop-counter baseline: drops we just discarded shouldn't
			// trigger another reset on the next block.
			lastDrops = state.dropCount.Load()
			log.Printf("[pipeline] flush: drained %d stale IQ buffers, DSP reset", drained)

		case rawBuf, ok := <-iqChan:
			if !ok {
				return
			}

			// If the SDR callback dropped any buffers since the last block, the
			// stream has a gap. Continuing with stale FIR/IIR history produces
			// audible warbling — reset state and skip this block.
			if d := state.dropCount.Load(); d != lastDrops {
				dropped := d - lastDrops
				lastDrops = d
				chain.Reset()
				log.Printf("[pipeline] %d IQ drops detected, DSP reset", dropped)
				continue
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
				if err := conn.SendDatagram(dgram); err == nil {
					fftSent++
				}
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

			// Soft limit to compress ADC-saturated signals (FM only)
			if chain.limiter != nil {
				chain.limiter.Process(audioSamples)
			}

			// Anti-aliased audio decimation to target rate
			if chain.audioDecimF != nil {
				audioSamples = chain.audioDecimF.Process(audioSamples)
			}

			// De-emphasis for FM modes
			if chain.deemph != nil {
				chain.deemph.Process(audioSamples)
			}

			// AM voice cleanup: bandpass 300-3000 Hz
			if chain.voiceHPF != nil {
				chain.voiceHPF.Process(audioSamples)
			}
			if chain.voiceLPF != nil {
				audioSamples = chain.voiceLPF.Process(audioSamples)
			}

			// Measure signal power for S-meter (before AGC)
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
			// Payload: [float32 smeter][float32 squelch][uint8 mode][uint64 freqHz]
			if time.Since(lastStatus) >= statusInterval {
				lastStatus = time.Now()
				state.mu.RLock()
				sq := state.squelchDb
				m := state.mode
				freq := state.freqHz
				state.mu.RUnlock()
				statusDgram := make([]byte, 18)
				statusDgram[0] = protocol.DatagramStatus
				copy(statusDgram[1:5], protocol.EncodeFloat32(signalPowerDb))
				copy(statusDgram[5:9], protocol.EncodeFloat32(sq))
				statusDgram[9] = byte(m)
				copy(statusDgram[10:18], protocol.EncodeUint64(freq))
				if err := conn.SendDatagram(statusDgram); err == nil {
					statusSent++
				}
			}

			// Emit seq-mark periodically so the client can compute loss.
			// Cheap (13 bytes/s) and crucial for any network adaptation.
			if time.Since(lastSeqMark) >= seqMarkInterval {
				lastSeqMark = time.Now()
				mark := make([]byte, 13)
				mark[0] = protocol.DatagramSeqMark
				binary.LittleEndian.PutUint32(mark[1:5], audioSent)
				binary.LittleEndian.PutUint32(mark[5:9], fftSent)
				binary.LittleEndian.PutUint32(mark[9:13], statusSent)
				_ = conn.SendDatagram(mark)
			}

			// Squelch gate: skip audio when signal is below threshold
			state.mu.RLock()
			squelchThreshold := state.squelchDb
			state.mu.RUnlock()
			if signalPowerDb < squelchThreshold {
				continue
			}

			// AGC: smooth gain control (regular AGC for FM, hang-time for AM).
			chain.gain.Process(audioSamples)

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
				if err := conn.SendDatagram(dgram); err == nil {
					audioSent++
				}
			}
		}
	}
}
