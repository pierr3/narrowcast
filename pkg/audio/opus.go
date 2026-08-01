// Package audio handles Opus encoding of demodulated audio.
package audio

import (
	"fmt"

	"gopkg.in/hraban/opus.v2"
)

// maxPacketBytes bounds one encoded frame. 20 ms at the bitrates used here is
// under 100 bytes; this is simply comfortably above anything libopus can emit
// for a single mono frame.
const maxPacketBytes = 1024

// OpusEncoder wraps the Opus encoder for streaming audio.
type OpusEncoder struct {
	enc        *opus.Encoder
	sampleRate int
	frameSize  int // samples per frame
	buf        []int16

	// Reused output storage, one entry per frame emitted in a single Encode
	// call, plus the returned slice itself. See Encode for the ownership rule.
	out     [][]byte
	packets [][]byte
}

// NewOpusEncoder creates an Opus encoder.
// sampleRate should match the demodulated audio rate (e.g., 16000 or 48000).
// bitrate is the target bitrate in bps (e.g., 32000).
// complexity is the analysis effort 0-10; see config.OpusComplexity for why the
// default is well below libopus's own.
func NewOpusEncoder(sampleRate, bitrate, complexity int) (*OpusEncoder, error) {
	enc, err := opus.NewEncoder(sampleRate, 1, opus.AppAudio)
	if err != nil {
		return nil, fmt.Errorf("opus encoder: %w", err)
	}
	if err := enc.SetBitrate(bitrate); err != nil {
		return nil, fmt.Errorf("opus set bitrate: %w", err)
	}
	if err := enc.SetComplexity(complexity); err != nil {
		return nil, fmt.Errorf("opus set complexity: %w", err)
	}
	// Enable in-band Forward Error Correction. The encoder hides a low-bitrate
	// copy of the previous frame inside the current one, so a single dropped
	// audio datagram can be reconstructed by the decoder instead of producing
	// a 20 ms silent gap. Critical on mobile/poor-wifi where packet loss is
	// common. Tell the encoder to assume ~10% loss so it allocates enough
	// bits to redundancy. Cost: ~20-25% bitrate overhead on the active frame.
	if err := enc.SetInBandFEC(true); err != nil {
		return nil, fmt.Errorf("opus set FEC: %w", err)
	}
	if err := enc.SetPacketLossPerc(10); err != nil {
		return nil, fmt.Errorf("opus set loss perc: %w", err)
	}

	// Opus frame size: 20ms worth of samples
	frameSize := sampleRate * 20 / 1000

	return &OpusEncoder{
		enc:        enc,
		sampleRate: sampleRate,
		frameSize:  frameSize,
		// Room for a full frame plus the remainder an input block that isn't an
		// exact multiple of frameSize leaves behind, so steady state never grows
		// this slice.
		buf: make([]int16, 0, 2*frameSize),
	}, nil
}

// FrameSize returns the number of float64 samples per Opus frame.
func (e *OpusEncoder) FrameSize() int {
	return e.frameSize
}

// Encode takes float64 audio samples [-1.0, 1.0] and returns complete Opus packets.
// May return zero packets if not enough samples have accumulated for a full frame.
//
// The returned slice and the packets in it are owned by the encoder and valid
// only until the next Encode call — the same convention the DSP stages use, and
// for the same reason: a 1 KB allocation per frame is 50 allocations a second of
// pure GC work on a Pi that has no cycles to spare. Callers must copy anything
// they keep. (The pipeline does: audioDatagram copies each packet into the
// datagram it builds.)
func (e *OpusEncoder) Encode(samples []float64) ([][]byte, error) {
	// Convert to int16 and buffer
	for _, s := range samples {
		// Clip
		if s > 1.0 {
			s = 1.0
		} else if s < -1.0 {
			s = -1.0
		}
		e.buf = append(e.buf, int16(s*32767))
	}

	e.packets = e.packets[:0]
	for len(e.buf) >= e.frameSize {
		out := e.frameBuf(len(e.packets))
		n, err := e.enc.Encode(e.buf[:e.frameSize], out)
		// Consume the frame either way: on error, retrying it next call would
		// fail identically and stall the buffer forever.
		e.consume(e.frameSize)
		if err != nil {
			return e.packets, fmt.Errorf("opus encode: %w", err)
		}
		e.packets = append(e.packets, out[:n])
	}
	return e.packets, nil
}

// frameBuf returns reusable output storage for the i'th frame of the current
// call. Frames within one call need distinct buffers, since they are all still
// referenced by the returned slice.
func (e *OpusEncoder) frameBuf(i int) []byte {
	for len(e.out) <= i {
		e.out = append(e.out, make([]byte, maxPacketBytes))
	}
	return e.out[i]
}

// consume drops the first n buffered samples, sliding the remainder to the
// front. Re-slicing instead (buf = buf[n:]) walks the base pointer forward, so
// every append would reallocate — which is where a third of the encoder's
// allocations used to come from.
func (e *OpusEncoder) consume(n int) {
	e.buf = e.buf[:copy(e.buf, e.buf[n:])]
}

// Reset clears the internal buffer.
func (e *OpusEncoder) Reset() {
	e.buf = e.buf[:0]
}

// SetBitrate retunes the encoder bitrate in bps. Safe to call mid-stream:
// libopus adjusts smoothly without resetting state. The pipeline uses this
// to drop fidelity when QualityReport indicates congestion (32 → 24 → 16k).
func (e *OpusEncoder) SetBitrate(bitrate int) error {
	return e.enc.SetBitrate(bitrate)
}

// SetComplexity sets the encoder's analysis effort, 0-10.
func (e *OpusEncoder) SetComplexity(c int) error {
	return e.enc.SetComplexity(c)
}

// Complexity reports the encoder's current analysis effort.
func (e *OpusEncoder) Complexity() (int, error) {
	return e.enc.Complexity()
}

// SetPacketLossPerc tells the encoder how aggressively to allocate bits to
// FEC redundancy. Higher values protect against more loss but spend more of
// the active-frame bitrate on the redundant copy. Safe to call mid-stream.
func (e *OpusEncoder) SetPacketLossPerc(pct int) error {
	return e.enc.SetPacketLossPerc(pct)
}
