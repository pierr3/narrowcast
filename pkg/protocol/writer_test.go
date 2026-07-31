package protocol

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeConn records datagrams handed to it. With a gate, every send waits until
// the gate is closed — the model of quic-go's SendDatagram blocking once its
// 32-frame queue is full, which is the situation Writer exists to survive.
type fakeConn struct {
	gate chan struct{}
	err  error

	mu   sync.Mutex
	sent [][]byte
}

func openConn() *fakeConn { return &fakeConn{} }

func gatedConn() *fakeConn { return &fakeConn{gate: make(chan struct{})} }

func (c *fakeConn) release() { close(c.gate) }

func (c *fakeConn) SendDatagram(b []byte) error {
	if c.gate != nil {
		<-c.gate
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, b)
	return c.err
}

func (c *fakeConn) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

// types returns the leading type byte of every delivered datagram, in order.
func (c *fakeConn) types() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, 0, len(c.sent))
	for _, d := range c.sent {
		out = append(out, d[0])
	}
	return out
}

// payloads returns the second byte of every delivered datagram, in order.
func (c *fakeConn) payloads() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, 0, len(c.sent))
	for _, d := range c.sent {
		out = append(out, d[1])
	}
	return out
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// The whole point of Writer: a stuck connection must never stall the producer.
func TestSendNeverBlocksOnStuckConnection(t *testing.T) {
	conn := gatedConn()
	w := NewWriter(conn)
	defer w.Close()

	done := make(chan struct{})
	go func() {
		// Far more than any queue depth.
		for i := 0; i < hiDepth*20; i++ {
			w.Send([]byte{DatagramAudio, byte(i)})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Send blocked while the connection was stuck")
	}

	drops, _ := w.Stats()
	if drops == 0 {
		t.Fatal("expected datagrams to be shed while the connection was stuck")
	}

	conn.release()
}

func TestSendDropsOldestAndKeepsNewest(t *testing.T) {
	conn := gatedConn()
	w := NewWriter(conn)
	defer w.Close()

	// One send is consumed by the writer goroutine and blocks in the
	// connection; the rest fill the queue.
	total := hiDepth + 10
	for i := 0; i < total; i++ {
		w.Send([]byte{DatagramAudio, byte(i)})
	}

	drops, _ := w.Stats()
	if drops == 0 {
		t.Fatalf("expected drops once the queue was full, got %d", drops)
	}

	conn.release()
	waitFor(t, func() bool { return conn.count() >= hiDepth })

	// The newest datagram must survive: for realtime audio, stale packets are
	// worthless, so the queue sheds from the front.
	got := conn.payloads()
	if want := byte(total - 1); got[len(got)-1] != want {
		t.Errorf("newest datagram not delivered: got payload %d, want %d", got[len(got)-1], want)
	}
}

// FFT frames must be shed before audio, and delivered after it.
func TestFFTIsLowerPriorityThanAudio(t *testing.T) {
	conn := gatedConn()
	w := NewWriter(conn)
	defer w.Close()

	// Fill both queues while the writer is stuck on its first send.
	w.Send([]byte{DatagramFFT, 0}) // consumed by the goroutine, blocks
	for i := 0; i < loDepth+5; i++ {
		w.Send([]byte{DatagramFFT, byte(i + 1)})
	}
	for i := 0; i < 4; i++ {
		w.Send([]byte{DatagramAudio, byte(i)})
	}

	conn.release()
	// loDepth FFT frames plus the four audio datagrams must all arrive; whether
	// a further frame was already in flight when the gate opened depends on
	// goroutine timing, so it isn't part of the expectation.
	waitFor(t, func() bool { return conn.count() >= loDepth+4 })

	// got[0] is whatever the writer goroutine picked up before it blocked, which
	// may legitimately be an FFT frame. Everything after that must show audio
	// draining completely before any queued FFT frame.
	got := conn.types()
	audio, fft, seenFFT := 0, 0, false
	for _, ty := range got[1:] {
		switch ty {
		case DatagramFFT:
			fft++
			seenFFT = true
		case DatagramAudio:
			audio++
			if seenFFT {
				t.Fatalf("audio delivered after a queued FFT frame: %v", got)
			}
		}
	}
	if audio == 0 || fft == 0 {
		t.Fatalf("expected both classes to be delivered, got %v", got)
	}

	if dropped, _ := w.Stats(); dropped == 0 {
		t.Error("expected FFT frames to be shed from the depth-2 low queue")
	}
}

func TestSendErrorsAreCountedNotFatal(t *testing.T) {
	conn := &fakeConn{err: errors.New("too large")}
	w := NewWriter(conn)
	defer w.Close()

	for i := 0; i < 3; i++ {
		w.Send([]byte{DatagramStatus})
	}
	waitFor(t, func() bool {
		_, errs := w.Stats()
		return errs == 3
	})
}

func TestSendIgnoresEmptyDatagram(t *testing.T) {
	conn := openConn()
	w := NewWriter(conn)
	defer w.Close()

	w.Send(nil)
	w.Send([]byte{})
	w.Send([]byte{DatagramStatus})

	waitFor(t, func() bool { return len(conn.types()) == 1 })
}

func TestCloseIsIdempotent(t *testing.T) {
	w := NewWriter(openConn())
	w.Close()
	w.Close()
}
