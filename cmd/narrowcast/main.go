package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof on http.DefaultServeMux
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
	// Fail fast on a configuration that would only show up as subtly wrong
	// audio at runtime (see config.Validate).
	if err := cfg.Validate(); err != nil {
		return err
	}

	if cfg.PProfAddr != "" {
		srv := &http.Server{Addr: cfg.PProfAddr, Handler: http.DefaultServeMux}
		go func() {
			log.Printf("[pprof] listening on %s", cfg.PProfAddr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("[pprof] %v", err)
			}
		}()
		defer srv.Close()
	}

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

	state := newServerState(ctx, cfg, dev)

	// Start SDR async read in a goroutine.
	go func() {
		bufSize := iqBufBytes(cfg.SampleRate)
		log.Printf("[sdr] async read: %d buffers of %d B (%.0f ms per block)",
			iqBufCount, bufSize, float64(bufSize)/2/float64(cfg.SampleRate)*1000)
		if err := dev.ReadAsync(state.onIQ, iqBufCount, bufSize); err != nil {
			log.Printf("[sdr] read async error: %v", err)
		}
	}()
	defer dev.CancelAsync()

	// Start QUIC server
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	srv, err := protocol.NewServer(addr, cfg.CertFile, cfg.KeyFile,
		func(clientCtx context.Context, conn quic.Connection) {
			handleClient(clientCtx, conn, state)
		})
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}
	defer srv.Close()

	log.Printf("[narrowcast] server starting on %s (freq=%d Hz, mode=%s, rate=%d)",
		addr, cfg.FrequencyHz, cfg.DemodMode, cfg.SampleRate)

	return srv.Serve(ctx)
}

const (
	// iqBufCount is how many USB transfer buffers librtlsdr keeps in flight.
	iqBufCount = 12
	// iqChanDepth bounds how much IQ can queue ahead of the pipeline. Eight
	// 20 ms blocks is 160 ms — enough to absorb a GC pause or a scheduling
	// hiccup, short enough that a real overload is reported as a drop (and
	// handled with a DSP reset) instead of quietly becoming latency.
	iqChanDepth = 8
	// iqBufPoolDepth sizes the recycled-buffer free list. Slightly larger than
	// iqChanDepth so a buffer is almost always available without allocating.
	iqBufPoolDepth = iqChanDepth + 4
)

// iqBufBytes picks the async read buffer size: ~20 ms of CU8 at the configured
// rate, rounded down to a multiple of 512.
//
// The multiple of 512 is not cosmetic: librtlsdr replaces any buf_len that
// isn't one with its own 256 KiB default, which at 960 kS/s is a 137 ms block.
// The previous code asked for sampleRate/10*2 and silently got that default.
func iqBufBytes(sampleRate int) int {
	const blockMs = 20
	b := sampleRate * 2 * blockMs / 1000
	b -= b % 512
	if b < 512 {
		b = 512
	}
	return b
}

// serverState holds the SDR, the shared DSP pipeline and the connected
// subscribers.
//
// One pipeline serves every subscriber. It used to be one pipeline per client,
// all of them reading the same iqChan, which meant two connections stole
// alternating IQ blocks from each other — both got half the sample stream and
// both produced broken audio — while also doing the DSP work twice. On the
// normal relay path there is only ever one connection so it went unnoticed,
// except during the overlap window of an uplink reconnect.
type serverState struct {
	cfg *config.Config
	dev sdr.SDRDevice
	// ctx is the server's lifetime. The pipeline is shared, so it must not be
	// tied to whichever client happened to start it.
	ctx context.Context

	mu        sync.RWMutex
	mode      protocol.DemodMode
	freqHz    uint64
	squelchDb float32

	// welcome is immutable after construction (it only reports static limits).
	welcome []byte

	subsMu sync.RWMutex
	subs   map[string]*protocol.Writer

	iqChan  chan []byte
	bufPool chan []byte
	// iqWanted gates the SDR callback. With no listeners there is nobody to
	// send to, so copying every buffer out of the USB ring is pure idle heat.
	iqWanted atomic.Bool
	// dropCount is incremented by the SDR callback whenever iqChan is full.
	// The pipeline tracks the last observed value and resets DSP state when it
	// increases, so we don't keep filtering across a discontinuity.
	dropCount atomic.Uint64

	// Pipeline control. Buffered so command handling never blocks on the
	// pipeline, and drained at pipeline start so a restart doesn't act on
	// commands from a previous run.
	modeChan    chan protocol.DemodMode
	flushChan   chan struct{}
	qualityChan chan qualityReport

	// Pipeline lifecycle, reference-counted over connections that sent Start.
	pipeMu    sync.Mutex
	pipeStop  chan struct{}
	pipeWg    sync.WaitGroup
	listeners int
}

func newServerState(ctx context.Context, cfg *config.Config, dev sdr.SDRDevice) *serverState {
	welcome := []byte{protocol.CmdWelcome, protocol.ProtoVersion}
	welcome = append(welcome, protocol.EncodeUint64(24_000_000)...)    // min freq
	welcome = append(welcome, protocol.EncodeUint64(1_766_000_000)...) // max freq
	welcome = append(welcome, protocol.EncodeFloat32(float32(cfg.SampleRate))...)

	return &serverState{
		cfg:         cfg,
		dev:         dev,
		ctx:         ctx,
		mode:        cfg.DemodMode,
		freqHz:      cfg.FrequencyHz,
		squelchDb:   cfg.SquelchDBm,
		welcome:     welcome,
		subs:        make(map[string]*protocol.Writer),
		iqChan:      make(chan []byte, iqChanDepth),
		bufPool:     make(chan []byte, iqBufPoolDepth),
		modeChan:    make(chan protocol.DemodMode, 1),
		flushChan:   make(chan struct{}, 1),
		qualityChan: make(chan qualityReport, 4),
	}
}

// onIQ is the SDR read callback. It runs on librtlsdr's USB thread, so it must
// never block: the buffer it is handed is reused as soon as it returns.
func (s *serverState) onIQ(buf []byte) {
	if !s.iqWanted.Load() {
		return
	}
	cp := s.takeBuf(len(buf))
	copy(cp, buf)
	select {
	case s.iqChan <- cp:
	default:
		// Pipeline can't keep up — drop and signal so it resets DSP state.
		s.dropCount.Add(1)
		s.recycleBuf(cp)
	}
}

func (s *serverState) takeBuf(n int) []byte {
	select {
	case b := <-s.bufPool:
		if cap(b) >= n {
			return b[:n]
		}
	default:
	}
	return make([]byte, n)
}

func (s *serverState) recycleBuf(b []byte) {
	select {
	case s.bufPool <- b[:cap(b)]:
	default: // pool full; let the GC have it
	}
}

func (s *serverState) drainIQ() int {
	drained := 0
	for {
		select {
		case b := <-s.iqChan:
			s.recycleBuf(b)
			drained++
		default:
			return drained
		}
	}
}

func (s *serverState) addSubscriber(id string, w *protocol.Writer) {
	s.subsMu.Lock()
	s.subs[id] = w
	s.subsMu.Unlock()
}

func (s *serverState) removeSubscriber(id string) {
	s.subsMu.Lock()
	delete(s.subs, id)
	s.subsMu.Unlock()
}

// broadcast hands a datagram to every subscriber.
//
// Holding the read lock across the sends is safe precisely because
// protocol.Writer.Send never blocks — it queues or drops. The same backing
// slice reaching several writers is also fine: nobody mutates it, and quic-go
// copies the payload into the datagram frame.
func (s *serverState) broadcast(dgram []byte) {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()
	for _, w := range s.subs {
		w.Send(dgram)
	}
}

// addListener registers interest in the audio stream, starting the shared
// pipeline on the first one.
func (s *serverState) addListener() {
	s.pipeMu.Lock()
	defer s.pipeMu.Unlock()

	s.listeners++
	if s.listeners > 1 {
		return
	}

	stop := make(chan struct{})
	s.pipeStop = stop
	s.iqWanted.Store(true)
	s.pipeWg.Add(1)
	go func() {
		defer s.pipeWg.Done()
		runPipeline(s, stop)
	}()
	log.Printf("[pipeline] started (1 listener)")
}

// removeListener drops interest, stopping the pipeline and the IQ copy path
// when the last listener goes away.
//
// pipeMu is held across pipeWg.Wait so a reconnect arriving mid-teardown can't
// start a second pipeline alongside the one still draining. The pipeline never
// takes pipeMu, so this cannot deadlock.
func (s *serverState) removeListener() {
	s.pipeMu.Lock()
	defer s.pipeMu.Unlock()

	if s.listeners == 0 {
		return
	}
	s.listeners--
	if s.listeners > 0 {
		return
	}

	stop := s.pipeStop
	s.pipeStop = nil
	s.iqWanted.Store(false)
	if stop != nil {
		close(stop)
	}
	s.pipeWg.Wait()
	s.drainIQ()
	log.Printf("[pipeline] stopped (no listeners)")
}

func handleClient(ctx context.Context, conn quic.Connection, state *serverState) {
	remote := conn.RemoteAddr().String()

	// All traffic is datagrams: [uint8 type][payload...]. Sends go through a
	// Writer so a congested link can never block this goroutine or, worse, the
	// shared pipeline behind it.
	w := protocol.NewWriter(conn)
	defer func() {
		if drops, errs := w.Stats(); drops > 0 || errs > 0 {
			log.Printf("[client %s] writer shed %d datagrams, %d send errors", remote, drops, errs)
		}
		w.Close()
	}()

	state.addSubscriber(remote, w)
	defer state.removeSubscriber(remote)

	// listening tracks whether this connection has an outstanding Start. The
	// relay multiplexes every phone onto one connection, so repeated Starts
	// from different phones must count once and a single Stop must end it.
	listening := false
	defer func() {
		if listening {
			state.removeListener()
		}
	}()

	w.Send(state.welcome)
	log.Printf("[client %s] sent Welcome datagram", remote)

	for {
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
			// Resend Welcome on every Hello (needed for the relay: the initial
			// Welcome may have been sent before any client was connected).
			w.Send(state.welcome)

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
			notify(state.flushChan)
			log.Printf("[client %s] freq → %d Hz", remote, hz)

		case protocol.CmdSetMode:
			if len(payload) < 1 {
				continue
			}
			mode := protocol.DemodMode(payload[0])
			state.mu.Lock()
			state.mode = mode
			state.mu.Unlock()
			replace(state.modeChan, mode)
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
			replace(state.qualityChan, qualityReport{
				audioLossPct: payload[0],
				fftLossPct:   payload[1],
			})

		case protocol.CmdPing:
			// Echo the token verbatim; the client times the round trip. Cheap
			// enough to answer unconditionally (5 B in, 5 B out, ~0.5 Hz).
			if len(payload) < 4 {
				continue
			}
			pong := make([]byte, 5)
			pong[0] = protocol.DatagramPong
			copy(pong[1:], payload[:4])
			w.Send(pong)

		case protocol.CmdStart:
			if listening {
				continue
			}
			listening = true
			state.addListener()
			log.Printf("[client %s] streaming started", remote)

		case protocol.CmdStop:
			if !listening {
				continue
			}
			listening = false
			state.removeListener()
			log.Printf("[client %s] streaming stopped", remote)
		}
	}
}

// notify posts a signal on a capacity-1 channel, coalescing with any pending
// one. Never blocks.
func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// replace posts v on a buffered channel, discarding an older queued value if
// the channel is full. Never blocks: for tuning and quality reports staleness
// is worse than a gap, so the newest value wins.
func replace[T any](ch chan T, v T) {
	select {
	case ch <- v:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- v:
	default:
	}
}

// drain empties a buffered channel.
func drain[T any](ch chan T) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// gainStage is satisfied by any AGC variant — both pkg/dsp.AGC (FM) and
// pkg/dsp.AudioAGC (AM hang-time) implement these.
//
// Reset discards state across a discontinuity (retune, dropped IQ). Restart
// marks a new transmission, which is a different event: the stream is intact,
// but the station on it has changed and its level may be nothing like the last
// one's.
type gainStage interface {
	Process([]float64)
	Reset()
	Restart()
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
// Bands deliberately wide so a single noisy QualityReport (especially on a
// flaky LAN wifi) doesn't yank the rate around.
//
//	 < 3 %  →  full (configured rate)
//	 3-15 %→  half
//	15-30 %→  1/5
//	 >30 % →  1/10
func adaptFFTInterval(base time.Duration, lossPct byte) time.Duration {
	switch {
	case lossPct < 3:
		return base
	case lossPct < 15:
		return base * 2
	case lossPct < 30:
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
// Bands widened from the original (2/5/floor) split so a single 5% spike on
// a flaky LAN wifi doesn't dump straight to the floor — the widened middle
// tier (24 kbps) absorbs short-burst loss while keeping audio intelligible.
// Combined with the min(current, prev) hysteresis at the call site, single
// outlier samples can no longer drive a step-down on their own.
//
//	 < 3 %  →  configured (default 32 kbps)
//	 3-12 %→  24 kbps
//	12-25 %→  20 kbps
//	 >25 % →  16 kbps  (floor)
func adaptOpusBitrate(base int, lossPct byte) int {
	const floor = 16000
	if base < floor {
		base = floor
	}
	switch {
	case lossPct < 3:
		return base
	case lossPct < 12:
		if base > 24000 {
			return 24000
		}
		return base
	case lossPct < 25:
		if base > 20000 {
			return 20000
		}
		return base
	default:
		return floor
	}
}

func minByte(a, b byte) byte {
	if a < b {
		return a
	}
	return b
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

// dspChain holds the DSP objects for one demod mode, plus the scratch buffers
// the pipeline reuses across blocks. Every stage that emits a slice owns and
// recycles it (see the pkg/dsp package comment), so a steady-state block costs
// no allocations at all.
type dspChain struct {
	xlat        *dsp.XlatingFilter
	fmDemod     *dsp.FMDemodulator // non-nil for NFM/WFM
	amDemod     *dsp.AMDemodulator // non-nil for AM
	demodFn     func([]complex128) []float64
	deemph      *dsp.DeEmphasis
	audioDecimF *dsp.RealFIRFilter // anti-aliased decimation filter (nil if no decimation needed)
	voiceHPF    *dsp.HighPassIIR   // voice bandpass high-pass (AM only)
	voiceLPF    *dsp.RealFIRFilter // voice bandpass low-pass (AM only)
	presence    *dsp.PresenceEQ    // consonant-band lift (AM only)
	limiter     *dsp.SoftLimiter   // soft clipper for ADC saturation
	gain        gainStage          // AGC (FM) or hang-time AudioAGC (AM)
	squelch     *dsp.Squelch       // gates on channel power, not audio level
	opusEnc     *audio.OpusEncoder
	audioRate   int

	// Carrier tracking (AM only; nil otherwise). The wide channel filter
	// captures offset-carrier ground stations, then fineTune shifts whichever
	// carrier is actually transmitting to DC and filters narrowly around it.
	fineTune        *dsp.FineTuner
	channelRate     float64
	carrierSearchHz float64
	carrierFFT      []complex128
	carrierWindow   *dsp.HannWindow
	lastCarrierScan time.Time
	pendingOffset   float64

	// dcBlock removes the tuner's LO leakage from the raw IQ before anything
	// reads it. See dsp.IQDCBlocker for why that matters to four stages at once.
	dcBlock *dsp.IQDCBlocker

	// Scratch, reused every block.
	iq       []complex128 // CU8 → complex
	hann     *dsp.HannWindow
	fftFrame []complex128 // windowed copy, FFT works in place
	fftBins  []byte       // pooled magnitude bins
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
	if c.presence != nil {
		c.presence.Reset()
	}
	if c.gain != nil {
		c.gain.Reset()
	}
	if c.squelch != nil {
		c.squelch.Reset()
	}
	if c.fineTune != nil {
		c.fineTune.Reset()
		// A retune means the old carrier offset describes a different channel.
		c.fineTune.SetOffset(0)
		c.pendingOffset = 0
		c.lastCarrierScan = time.Time{}
	}
	if c.dcBlock != nil {
		// The offset belongs to the tuner at its current frequency, so a
		// retune invalidates the estimate rather than merely disturbing it.
		c.dcBlock.Reset()
	}
	if c.opusEnc != nil {
		c.opusEnc.Reset()
	}
}

// Carrier tracking tuning constants.
const (
	// carrierFFTSize is the scan FFT length, run on the channel stream. At the
	// 48 kHz AM channel rate that's ~94 Hz per bin — far finer than needed,
	// since AM detection is non-coherent, and cheap at a few scans per second.
	carrierFFTSize = 512
	// amFilterTaps sets the narrow AM filter's transition width. Stopband depth
	// comes from the window (~53 dB for Hamming) and does not improve with more
	// taps, so this only has to be enough to put an 8.33 kHz neighbour past the
	// transition: at 48 kHz, 65 taps gives ~2.4 kHz of transition, so the
	// stopband starts around 6 kHz — comfortably below the neighbour at 8.3 kHz.
	//
	// It is paid twice (once per component) at every output, so this is the most
	// expensive number in the AM chain; 161 taps measured *more* costly than the
	// entire wideband channel filter.
	amFilterTaps = 65

	// Presence lift geometry. 2 kHz sits in the middle of the consonant band,
	// and Q 0.9 spreads the boost across roughly 1.2-3.3 kHz — matched to what
	// the 400-3000 Hz voice bandpass actually passes, so none of it is spent
	// on frequencies that get removed straight afterwards.
	amPresenceHz = 2000.0
	amPresenceQ  = 0.9

	// How often to look for the carrier. Ground stations don't move, so this
	// only has to catch a *different* station keying up.
	carrierScanInterval = 200 * time.Millisecond
	// Two consecutive scans must agree within this to be believed, which stops
	// a noise peak from dragging the tuning around.
	carrierAgreeHz = 600
	// Don't bother retuning for less than this; AM detection is non-coherent so
	// a few hundred Hz of residual offset is inaudible.
	carrierMoveHz = 400
)

// airbandSearchHz says how far either side of centre to look for a carrier,
// derived from the channel plan the tuned frequency belongs to.
//
// The window has to come from the plan, not be a constant. On a 25 kHz channel
// the nearest other channel is 25 kHz away and offset-carrier transmitters sit
// up to ±7.5 kHz out, so a wide search is both safe and necessary. On an 8.33
// channel the neighbours are only 8.33 kHz away — search that wide and the
// tuner would happily lock onto the next channel's traffic.
//
// Carriers on the 8.33 grid are at multiples of 25000/3 Hz, so they don't divide
// 25000 exactly; that's what distinguishes the two plans from the frequency
// alone. Returns 0 to disable tracking outside the airband.
func airbandSearchHz(freqHz uint64) float64 {
	const (
		airbandLow  = 118_000_000
		airbandHigh = 137_000_000
	)
	if freqHz < airbandLow || freqHz > airbandHigh {
		return 0
	}
	if freqHz%25_000 == 0 {
		return 10_000 // 25 kHz channel: covers ±7.5 kHz offset carriers
	}
	return 3_500 // 8.33 kHz channel: stay well inside the neighbours
}

// demodBlock runs one block of raw IQ through the chain: channel filter,
// squelch decision, AM carrier tracking, demodulation and audio cleanup. It
// returns the block's audio — owned by the chain and valid until the next call
// — along with the channel power the squelch and S-meter both read, and the
// gate decision. Audio is nil when the block produced none.
//
// AGC and Opus encoding stay in the pipeline rather than living here: both run
// only while the gate is open, so the caller needs the gate decision first.
//
// The IQ is expected to have had the tuner's DC offset removed already; the
// pipeline does that ahead of the spectrum FFT, which needs it too.
//
// Being callable without a pipeline, an SDR or a network is the point — it is
// what lets the demodulation chain be measured end to end.
func (c *dspChain) demodBlock(iq []complex128, squelchDb float64, tunedHz uint64, now time.Time) ([]float64, float64, bool) {
	channelIQ := c.xlat.Process(iq)
	if len(channelIQ) == 0 {
		return nil, 0, false
	}

	// Squelch and S-meter both read channel power, measured here on the
	// filtered RF channel before demodulation.
	//
	// This used to be the RMS of the demodulated *audio*, which is why the
	// threshold felt impossible to set: speech dips between syllables, so the
	// level fell below the line mid-sentence and chopped transmissions apart. An
	// AM carrier is steady for the whole transmission and FM is
	// constant-envelope, so channel power holds still while someone talks. The
	// meter reports the same quantity the gate uses, so aiming the slider at
	// what you see now works.
	channelPowerDb := dsp.ChannelPowerDb(channelIQ)
	c.squelch.SetThreshold(squelchDb)
	open := c.squelch.Update(channelPowerDb, len(channelIQ))

	// Carrier tracking + narrow filtering (AM only). Tracking reads the wide
	// stream above; only the audio path gets narrowed.
	if c.fineTune != nil {
		c.carrierSearchHz = airbandSearchHz(tunedHz)
		if open {
			c.trackCarrier(channelIQ, now)
		}
		channelIQ = c.fineTune.Process(channelIQ)
	}

	audio := c.demodFn(channelIQ)

	// Soft limit to compress ADC-saturated signals (FM only)
	if c.limiter != nil {
		c.limiter.Process(audio)
	}
	// Anti-aliased audio decimation to target rate
	if c.audioDecimF != nil {
		audio = c.audioDecimF.Process(audio)
	}
	// De-emphasis for FM modes
	if c.deemph != nil {
		c.deemph.Process(audio)
	}
	// AM voice cleanup: bandpass 400-3000 Hz
	if c.voiceHPF != nil {
		c.voiceHPF.Process(audio)
	}
	if c.voiceLPF != nil {
		audio = c.voiceLPF.Process(audio)
	}
	// Lift the consonants last, so it shapes the band the voice filter kept
	// rather than boosting energy that is about to be filtered away.
	if c.presence != nil {
		c.presence.Process(audio)
	}
	return audio, channelPowerDb, open
}

// trackCarrier looks for the transmitting carrier and points the fine tuner at
// it. Runs on the wide channel stream — deliberately, since the narrow filter
// downstream would reject an offset carrier and the tuner would never find it.
func (c *dspChain) trackCarrier(channel []complex128, now time.Time) {
	if c.fineTune == nil || c.carrierSearchHz <= 0 {
		return
	}
	if len(channel) < len(c.carrierFFT) {
		return
	}
	if now.Sub(c.lastCarrierScan) < carrierScanInterval {
		return
	}
	c.lastCarrierScan = now

	copy(c.carrierFFT, channel[len(channel)-len(c.carrierFFT):])
	c.carrierWindow.Apply(c.carrierFFT)
	dsp.FFT(c.carrierFFT)
	offset := dsp.FindCarrierOffset(c.carrierFFT, c.channelRate, c.carrierSearchHz)

	// Believe it only once two scans agree, then move only if it's worth it.
	if math.Abs(offset-c.pendingOffset) <= carrierAgreeHz {
		if math.Abs(offset-c.fineTune.OffsetHz()) > carrierMoveHz {
			log.Printf("[dsp] carrier at %+.0f Hz from centre — retuning fine filter", offset)
			c.fineTune.SetOffset(offset)
		}
	}
	c.pendingOffset = offset
}

func buildDSPChain(mode protocol.DemodMode, cfg *config.Config, opusBitrate int) (*dspChain, error) {
	sampleRate := cfg.SampleRate
	channelBW := mode.ChannelBandwidth()
	audioRate := mode.AudioRate()

	// Total decimation is exact: config.Validate guarantees the sample rate is
	// a multiple of every mode's audio rate.
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

	log.Printf("[dsp] mode=%s xlatDecim=%d audioDecim=%d taps=%d decimatedRate=%.0f audioRate=%d",
		mode, xlatDecim, audioDecim, numTaps, decimatedRate, audioRate)

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
	// and a gate causes crackle by chattering on AGC-amplified noise.
	var voiceHPF *dsp.HighPassIIR
	var voiceLPF *dsp.RealFIRFilter
	if mode == protocol.ModeAM {
		// 2nd-order high-pass at 400 Hz to kill carrier hum and rumble
		voiceHPF = dsp.NewHighPassIIR(400, float64(audioRate))
		// Low-pass at 3000 Hz to remove high-frequency noise
		lpfNumTaps := 65
		voiceTaps := dsp.NewLowPassFIR(3000, float64(audioRate), lpfNumTaps)
		voiceLPF = dsp.NewRealFIRDecimator(voiceTaps, 1) // decim=1, just filtering
		log.Printf("[dsp] AM voice cleanup: bandpass 400-3000 Hz")
	}

	// Presence lift, inside the band the voice filter passes. Placed before the
	// AGC so the level the AGC normalises is the level that actually leaves.
	var presence *dsp.PresenceEQ
	if mode == protocol.ModeAM && cfg.AMPresenceDb > 0 {
		presence = dsp.NewPresenceEQ(amPresenceHz, amPresenceQ, cfg.AMPresenceDb, float64(audioRate))
		log.Printf("[dsp] AM presence: +%.1f dB at %.0f Hz (Q %.1f)",
			cfg.AMPresenceDb, amPresenceHz, amPresenceQ)
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
		gain = dsp.NewAudioAGC(dsp.AMAGCTarget, 0.1, 0.001, 500.0, float64(audioRate), 0)
		log.Printf("[dsp] AM hang-time AudioAGC: target=%.2f hang=500ms", dsp.AMAGCTarget)
	} else {
		gain = dsp.NewAGC(-12, 30, 20.0, 500.0, float64(audioRate))
	}

	opusEnc, err := audio.NewOpusEncoder(audioRate, opusBitrate, cfg.OpusComplexity)
	if err != nil {
		return nil, err
	}

	// Squelch runs on the channel stream, so its hang time is measured at
	// decimatedRate. See dsp.Squelch for why it gates on channel power rather
	// than audio level.
	squelch := dsp.NewSquelch(
		float64(cfg.SquelchDBm), cfg.SquelchHysteresisDb, cfg.SquelchHangMs, decimatedRate)

	// AM carrier tracking. The wide channel filter above stays wide so
	// offset-carrier ground stations are captured at all; this stage then shifts
	// whichever carrier is transmitting to DC and filters narrowly around it,
	// which is where the hiss reduction comes from. See dsp.FineTuner.
	var fineTune *dsp.FineTuner
	var carrierFFT []complex128
	var carrierWindow *dsp.HannWindow
	if mode == protocol.ModeAM && cfg.AMCarrierTrack {
		// Decimate to the audio rate here instead of in a separate stage: the
		// narrow filter then computes audioDecim times fewer outputs, and
		// audioDecimF becomes redundant (its only job was anti-aliased
		// decimation, which this filter now does with a tighter cutoff).
		fineTune = dsp.NewFineTuner(cfg.AMHalfBandwidthHz, decimatedRate, amFilterTaps, audioDecim)
		audioDecimF = nil
		carrierFFT = make([]complex128, carrierFFTSize)
		carrierWindow = dsp.NewHannWindow(carrierFFTSize)
		log.Printf("[dsp] AM carrier tracking on, narrow filter ±%.0f Hz, scan FFT %d bins (%.0f Hz each)",
			cfg.AMHalfBandwidthHz, carrierFFTSize, decimatedRate/carrierFFTSize)
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
		presence:    presence,
		limiter:     limiter,
		gain:        gain,
		squelch:     squelch,
		fineTune:    fineTune,
		channelRate: decimatedRate,
		// The search window depends on the tuned frequency, so it's refreshed
		// per block in the pipeline rather than fixed here.
		carrierFFT:    carrierFFT,
		carrierWindow: carrierWindow,
		opusEnc:       opusEnc,
		audioRate:     audioRate,
		dcBlock:       dsp.NewIQDCBlocker(0),
		hann:          dsp.NewHannWindow(cfg.FFTSize),
		fftFrame:      make([]complex128, cfg.FFTSize),
		fftBins:       make([]byte, cfg.FFTBins),
	}, nil
}

// pipelineHealthInterval is how often the pipeline reports its own load.
const pipelineHealthInterval = 60 * time.Second

// pipelineHealth measures what fraction of the wall clock the pipeline spends
// working, which is the number that decides whether audio is late.
//
// It exists because the obvious measurements both lie. Total CPU on a four-core
// Pi reads ~25 % while this goroutine — a single thread doing all the DSP and
// the Opus encode — is completely saturated, and the SoC temperature says
// nothing about whether *this* thread made its deadline. Meanwhile "busy" is
// bimodal: only the Opus encoder runs while the squelch is open, so a channel
// that idles at 40 % can be over budget for the duration of every transmission,
// which is exactly when anyone is listening.
//
// Over budget does not mean dropped audio straight away — iqChan absorbs 8
// blocks, and because the pipeline is normally much faster than real time it
// drains that backlog quickly. What it does mean is that the backlog is standing
// latency for as long as the overload lasts, and once the queue is full the SDR
// callback starts dropping, which forces a DSP reset. Both are audible: the
// first as lag, the second as a break in the audio.
type pipelineHealth struct {
	// blockBudget is how much audio one IQ block represents, i.e. how long the
	// pipeline may take on it and still keep up.
	blockBudget time.Duration

	windowStart time.Time
	blocks      int
	busy        time.Duration
	worst       time.Duration
}

func newPipelineHealth(sampleRate int) *pipelineHealth {
	samples := iqBufBytes(sampleRate) / 2
	return &pipelineHealth{
		blockBudget: time.Duration(float64(samples) / float64(sampleRate) * float64(time.Second)),
		windowStart: time.Now(),
	}
}

func (h *pipelineHealth) observe(d time.Duration) {
	h.blocks++
	h.busy += d
	if d > h.worst {
		h.worst = d
	}
}

// report logs one line per interval and starts a new window. queued is the
// current iqChan depth — standing latency, in blocks.
func (h *pipelineHealth) report(now time.Time, queued, queueCap int, drops uint64) {
	elapsed := now.Sub(h.windowStart)
	if elapsed < pipelineHealthInterval || h.blocks == 0 {
		return
	}

	busyPct := float64(h.busy) / float64(elapsed) * 100
	mean := h.busy / time.Duration(h.blocks)
	// Anything past ~70 % of one core has no room left for the transmission
	// that hasn't started yet; a single block over budget already queued.
	level := "health"
	if busyPct > 70 || h.worst > h.blockBudget {
		level = "HEALTH WARNING"
	}
	log.Printf("[pipeline] %s: %.0f%% of one core, %d blocks, mean %.2f ms, worst %.2f ms "+
		"(budget %.1f ms), iq queue %d/%d, drops %d",
		level, busyPct, h.blocks,
		float64(mean)/float64(time.Millisecond),
		float64(h.worst)/float64(time.Millisecond),
		float64(h.blockBudget)/float64(time.Millisecond),
		queued, queueCap, drops)

	h.windowStart = now
	h.blocks = 0
	h.busy = 0
	h.worst = 0
}

// runPipeline reads IQ data, demodulates, encodes, and broadcasts datagrams to
// every subscriber. One instance runs while at least one listener is present.
func runPipeline(state *serverState, stop <-chan struct{}) {
	cfg := state.cfg
	state.mu.RLock()
	mode := state.mode
	state.mu.RUnlock()

	fftSize := cfg.FFTSize
	baseFFTInterval := time.Second / time.Duration(cfg.FFTRate)
	baseOpusBitrate := cfg.OpusBitrate

	chain, err := buildDSPChain(mode, cfg, baseOpusBitrate)
	if err != nil {
		log.Printf("[pipeline] dsp chain error: %v", err)
		return
	}

	// Commands queued while the pipeline was stopped describe a world we
	// already read from state above, so start from a clean slate.
	drain(state.modeChan)
	drain(state.flushChan)
	drain(state.qualityChan)

	// Track SDR drop count to detect IQ-buffer gaps. On increase we reset all
	// DSP state and skip the next block to avoid filtering across a
	// discontinuity (which sounds like warbling artifacts).
	lastDrops := state.dropCount.Load()

	// Adaptive state. fftInterval and currentBitrate start at configured
	// defaults and step down only when QualityReport indicates loss.
	fftInterval := baseFFTInterval
	currentBitrate := baseOpusBitrate
	// Hysteresis: keep the previous report's loss values so a single outlier
	// sample (a brief wifi blip on an otherwise-clean LAN) can't drive a
	// step-down on its own. We adapt against min(current, prev), so degrading
	// requires two consecutive bad reports while recovery happens immediately
	// on the first good one.
	var prevAudioLossPct, prevFFTLossPct byte

	// Sequence counters — included in DatagramSeqMark every second. The client
	// diffs these against its own receive counts to compute loss. They count
	// datagrams handed to the Writer, including any it later sheds, which is
	// what we want: locally dropped datagrams are real loss from the client's
	// point of view and should feed the adaptation loop.
	var audioSent, fftSent, statusSent uint32
	// audioSeq tags each Opus frame so the client can tell a lost packet from
	// silence and spend the in-band FEC. Wraps at 2^16 by design.
	var audioSeq uint16
	const seqMarkInterval = 1 * time.Second
	lastSeqMark := time.Now()

	lastFFT := time.Now()

	// 10 Hz status. Payload is ~18-19 B (incl. the relay's client-count
	// append), so this is ~190 B/s. The client animates between samples, so a
	// faster rate buys no perceived smoothness — it only costs the client a
	// SwiftUI invalidation per frame, which is main-thread work on a phone.
	lastStatus := time.Now()
	const statusInterval = 100 * time.Millisecond
	var signalPowerDb float32 = -120

	// squelchOpen is this block's gate decision; wasOpen tracks the previous one
	// so the Opus encoder is reset exactly once on close — without that, samples
	// buffered before the gap get prepended to the next transmission as a click.
	squelchOpen := false
	wasOpen := false

	fftTooSmallLogged := false

	health := newPipelineHealth(cfg.SampleRate)
	// blockStart is set when a block is dequeued and settled at the top of the
	// next iteration. Timing it there rather than at the end of the case body is
	// what makes the early `continue` paths count too — a drop reset, an empty
	// filter output and a closed squelch all exit that way, and they are the
	// cheap paths, so leaving them out would flatter the average badly.
	var blockStart time.Time

	for {
		if !blockStart.IsZero() {
			now := time.Now()
			health.observe(now.Sub(blockStart))
			blockStart = time.Time{}
			health.report(now, len(state.iqChan), cap(state.iqChan), state.dropCount.Load())
		}

		select {
		case <-state.ctx.Done():
			return
		case <-stop:
			return

		case newMode := <-state.modeChan:
			if newMode == mode {
				continue
			}
			newChain, err := buildDSPChain(newMode, cfg, currentBitrate)
			if err != nil {
				log.Printf("[pipeline] rebuild dsp chain: %v", err)
				continue
			}
			chain = newChain
			mode = newMode
			log.Printf("[pipeline] DSP chain rebuilt for mode %s", mode)

		case qr := <-state.qualityChan:
			// Client reported its loss measurement. Adapt FFT rate and Opus
			// bitrate so the audio path stays viable on a struggling network.
			//
			// Use min(current, previous) as the stepping signal: an isolated
			// spike sees a low previous value and gets filtered out, but a
			// sustained problem keeps both samples high and steps the rate
			// down. FEC redundancy uses the live (un-filtered) value so the
			// encoder still allocates protection bits for an active burst.
			adaptAudioLoss := minByte(qr.audioLossPct, prevAudioLossPct)
			adaptFFTLoss := minByte(qr.fftLossPct, prevFFTLossPct)
			prevAudioLossPct = qr.audioLossPct
			prevFFTLossPct = qr.fftLossPct

			newFFTInterval := adaptFFTInterval(baseFFTInterval, adaptFFTLoss)
			newBitrate := adaptOpusBitrate(baseOpusBitrate, adaptAudioLoss)
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

		case <-state.flushChan:
			// Hardware was retuned. Drop any IQ buffered before the retune
			// (it's at the OLD frequency) and reset every stateful DSP stage
			// so post-retune audio doesn't carry a pre-retune transient.
			drained := state.drainIQ()
			chain.Reset()
			// Sync the drop-counter baseline: drops we just discarded
			// shouldn't trigger another reset on the next block.
			lastDrops = state.dropCount.Load()
			log.Printf("[pipeline] flush: drained %d stale IQ buffers, DSP reset", drained)

		case rawBuf, ok := <-state.iqChan:
			if !ok {
				return
			}
			blockStart = time.Now()

			// If the SDR callback dropped any buffers since the last block, the
			// stream has a gap. Continuing with stale FIR/IIR history produces
			// audible warbling — reset state and skip this block.
			if d := state.dropCount.Load(); d != lastDrops {
				dropped := d - lastDrops
				lastDrops = d
				chain.Reset()
				state.recycleBuf(rawBuf)
				log.Printf("[pipeline] %d IQ drops detected, DSP reset", dropped)
				continue
			}

			// Convert CU8 → complex into chain-owned scratch.
			iq := dsp.CU8ToComplexInto(chain.iq, rawBuf)
			chain.iq = iq
			state.recycleBuf(rawBuf)

			// Strip the tuner's LO leakage before anything reads the block —
			// the spectrum, the S-meter, the squelch and the carrier tracker
			// are all misled by it, each in its own way. The estimate itself is
			// only refined on blocks with the gate shut, further down.
			chain.dcBlock.Process(iq)

			// --- FFT waterfall (on raw wideband IQ) ---
			// The FFT window is simply the tail of the current block: any
			// contiguous fftSize samples are a valid snapshot, so there is no
			// reason to accumulate (the old code appended every block into a
			// growing buffer and threw away 99 % of it).
			if len(iq) >= fftSize {
				if time.Since(lastFFT) >= fftInterval {
					lastFFT = time.Now()

					copy(chain.fftFrame, iq[len(iq)-fftSize:])
					chain.hann.Apply(chain.fftFrame)
					dsp.FFT(chain.fftFrame)
					bins := dsp.MagnitudeToBins(chain.fftBins, chain.fftFrame, cfg.FFTBins)
					chain.fftBins = bins

					// [type][uint16 numBins BE][bins...]
					dgram := make([]byte, 3+len(bins))
					dgram[0] = protocol.DatagramFFT
					dgram[1] = byte(len(bins) >> 8)
					dgram[2] = byte(len(bins))
					copy(dgram[3:], bins)
					state.broadcast(dgram)
					fftSent++
				}
			} else if !fftTooSmallLogged {
				fftTooSmallLogged = true
				log.Printf("[pipeline] IQ block (%d samples) smaller than fftsize (%d) — spectrum disabled",
					len(iq), fftSize)
			}

			state.mu.RLock()
			squelchThreshold := float64(state.squelchDb)
			tunedHz := state.freqHz
			state.mu.RUnlock()

			audioSamples, channelPowerDb, open := chain.demodBlock(
				iq, squelchThreshold, tunedHz, time.Now())
			if audioSamples == nil {
				continue
			}
			signalPowerDb = float32(channelPowerDb)
			squelchOpen = open

			// Refine the DC estimate only on quiet blocks. A carrier tuned dead
			// on frequency also sits at DC, so adapting through a transmission
			// would subtract the signal itself. Leakage drifts over minutes and
			// transmissions last seconds, so holding still costs nothing.
			if !squelchOpen || chain.dcBlock.Priming() {
				chain.dcBlock.Refine(iq)
			}

			// Send status datagram periodically.
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
				state.broadcast(statusDgram)
				statusSent++
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
				state.broadcast(mark)
			}

			// Squelch gate — decided above from channel power, with hysteresis
			// and hang time (see dsp.Squelch).
			if !squelchOpen {
				if wasOpen {
					wasOpen = false
					// Drop the partial frame buffered in the encoder so it
					// isn't prepended to the next transmission.
					chain.opusEnc.Reset()
				}
				continue
			}
			if !wasOpen {
				wasOpen = true
				// A new transmission, not a continuation. On the airband the
				// previous one might have been an aircraft directly overhead and
				// this one a distant ground station 20 dB down; carrying the old
				// gain over leaves the quiet one barely audible until hang and
				// decay have run their course.
				chain.gain.Restart()
			}

			// AGC: smooth gain control (regular AGC for FM, hang-time for AM).
			chain.gain.Process(audioSamples)

			// Opus encode
			packets, err := chain.opusEnc.Encode(audioSamples)
			if err != nil {
				log.Printf("[pipeline] opus: %v", err)
				continue
			}

			for _, pkt := range packets {
				state.broadcast(audioDatagram(cfg.AudioSeq, audioSeq, pkt))
				audioSeq++
				audioSent++
			}
		}
	}
}

// audioDatagram wraps an Opus packet for the wire, with or without a sequence
// number. See protocol.DatagramAudioSeq for why both forms exist.
func audioDatagram(withSeq bool, seq uint16, pkt []byte) []byte {
	if !withSeq {
		dgram := make([]byte, 1+len(pkt))
		dgram[0] = protocol.DatagramAudio
		copy(dgram[1:], pkt)
		return dgram
	}
	dgram := make([]byte, 3+len(pkt))
	dgram[0] = protocol.DatagramAudioSeq
	binary.LittleEndian.PutUint16(dgram[1:3], seq)
	copy(dgram[3:], pkt)
	return dgram
}
