// Package audio handles Opus encoding of demodulated audio.
package audio

import (
	"fmt"

	"gopkg.in/hraban/opus.v2"
)

// OpusEncoder wraps the Opus encoder for streaming audio.
type OpusEncoder struct {
	enc        *opus.Encoder
	sampleRate int
	frameSize  int // samples per frame
	buf        []int16
}

// NewOpusEncoder creates an Opus encoder.
// sampleRate should match the demodulated audio rate (e.g., 16000 or 48000).
// bitrate is the target bitrate in bps (e.g., 32000).
func NewOpusEncoder(sampleRate, bitrate int) (*OpusEncoder, error) {
	enc, err := opus.NewEncoder(sampleRate, 1, opus.AppAudio)
	if err != nil {
		return nil, fmt.Errorf("opus encoder: %w", err)
	}
	if err := enc.SetBitrate(bitrate); err != nil {
		return nil, fmt.Errorf("opus set bitrate: %w", err)
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
		buf:        make([]int16, 0, frameSize),
	}, nil
}

// FrameSize returns the number of float64 samples per Opus frame.
func (e *OpusEncoder) FrameSize() int {
	return e.frameSize
}

// Encode takes float64 audio samples [-1.0, 1.0] and returns complete Opus packets.
// May return zero packets if not enough samples have accumulated for a full frame.
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

	var packets [][]byte
	for len(e.buf) >= e.frameSize {
		frame := e.buf[:e.frameSize]
		e.buf = e.buf[e.frameSize:]

		// Max Opus packet size
		out := make([]byte, 1024)
		n, err := e.enc.Encode(frame, out)
		if err != nil {
			return packets, fmt.Errorf("opus encode: %w", err)
		}
		packets = append(packets, out[:n])
	}
	return packets, nil
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

// SetPacketLossPerc tells the encoder how aggressively to allocate bits to
// FEC redundancy. Higher values protect against more loss but spend more of
// the active-frame bitrate on the redundant copy. Safe to call mid-stream.
func (e *OpusEncoder) SetPacketLossPerc(pct int) error {
	return e.enc.SetPacketLossPerc(pct)
}
