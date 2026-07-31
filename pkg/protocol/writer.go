package protocol

import (
	"log"
	"sync"
	"sync/atomic"
)

// Datagrammer is the slice of quic.Connection that Writer needs. Narrowing it
// to one method keeps Writer unit-testable without a live QUIC stack.
type Datagrammer interface {
	SendDatagram([]byte) error
}

// Queue depths. hiDepth ≈ 1 s of audio at 50 packets/s — deep enough to ride
// out a scheduling hiccup, shallow enough that a genuinely congested link
// sheds old audio instead of accumulating a latency backlog.
//
// loDepth is 2 because a stale spectrum frame has no value: if the link can't
// keep up, the newest frame is the only one worth sending.
const (
	hiDepth = 64
	loDepth = 2
)

// Writer serializes datagram sends for one QUIC connection and never blocks
// the caller.
//
// This exists because quic-go's Connection.SendDatagram blocks once 32
// datagram frames are queued (see datagram_queue.go: "Once that limit is
// reached, Add blocks until the queue size has reduced"). Every producer in
// this system is realtime — the SDR pipeline, the relay fan-out, the uplink
// bridge — and for all of them blocking is the wrong answer:
//
//	pipeline blocks in SendDatagram
//	  → stops draining iqChan
//	  → SDR callback drops buffers
//	  → pipeline resets DSP state and skips blocks
//	  → a 300 ms network hiccup becomes seconds of dropped audio
//
// In the relay the same stall propagates further: one slow client used to
// block the fan-out loop, which stopped reading from upstream, which blocked
// the uplink, which blocked the Pi's pipeline. One bad phone froze everyone.
//
// Writer moves the blocking send into its own goroutine and gives producers a
// Send that drops the oldest queued datagram rather than waiting. Loss is
// already the design assumption on this path (unreliable datagrams, Opus FEC,
// client-side loss reporting), so shedding is strictly better than stalling.
type Writer struct {
	conn Datagrammer

	hi   chan []byte // audio, status, seq-marks, commands — shed last
	lo   chan []byte // FFT frames — shed first
	done chan struct{}

	closeOnce sync.Once
	drops     atomic.Uint64
	errs      atomic.Uint64
}

// NewWriter starts the writer goroutine for conn. Call Close to stop it.
func NewWriter(conn Datagrammer) *Writer {
	w := &Writer{
		conn: conn,
		hi:   make(chan []byte, hiDepth),
		lo:   make(chan []byte, loDepth),
		done: make(chan struct{}),
	}
	go w.run()
	return w
}

// Send queues a datagram. Never blocks. If the queue for this datagram's
// priority class is full, the oldest queued datagram is discarded to make
// room and the drop counter is incremented.
//
// The caller must not mutate dgram afterwards; the same slice may legitimately
// be handed to several Writers (relay fan-out does exactly that), and quic-go
// copies the payload into the frame before sending.
func (w *Writer) Send(dgram []byte) {
	if len(dgram) == 0 {
		return
	}
	q := w.hi
	if dgram[0] == DatagramFFT {
		q = w.lo
	}
	if !offer(q, dgram) {
		w.drops.Add(1)
	}
}

// Close stops the writer goroutine. Safe to call more than once; queued
// datagrams are abandoned (they are realtime data, so replaying them on the
// way out would be worse than dropping them).
func (w *Writer) Close() {
	w.closeOnce.Do(func() { close(w.done) })
}

// Stats reports cumulative dropped datagrams and send errors. Useful for
// logging on disconnect — a non-zero drop count means the link couldn't keep
// up with the configured bitrate.
func (w *Writer) Stats() (drops, errs uint64) {
	return w.drops.Load(), w.errs.Load()
}

func (w *Writer) run() {
	for {
		// Strict priority: drain hi before ever looking at lo, so FFT frames
		// can't delay audio behind them.
		select {
		case <-w.done:
			return
		case b := <-w.hi:
			w.emit(b)
			continue
		default:
		}

		select {
		case <-w.done:
			return
		case b := <-w.hi:
			w.emit(b)
		case b := <-w.lo:
			w.emit(b)
		}
	}
}

func (w *Writer) emit(dgram []byte) {
	if err := w.conn.SendDatagram(dgram); err != nil {
		// Log the first failure and then sparsely: a dead connection would
		// otherwise fill the journal at datagram rate. DatagramTooLargeError
		// on a reduced-MTU path is the interesting case to see here.
		if n := w.errs.Add(1); n == 1 || n%1000 == 0 {
			log.Printf("[writer] send datagram type 0x%02x (%d B): %v (error %d)",
				dgram[0], len(dgram), err, n)
		}
	}
}

// offer pushes b onto ch, discarding the oldest entry if ch is full. Returns
// false if anything had to be dropped.
func offer(ch chan []byte, b []byte) bool {
	select {
	case ch <- b:
		return true
	default:
	}
	// Make room. Best-effort: with concurrent producers another goroutine may
	// win the slot we just freed, in which case we drop b itself. Either way
	// exactly one datagram is lost and we never block.
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- b:
	default:
	}
	return false
}
