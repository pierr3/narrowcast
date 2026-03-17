// Package protocol defines the Narrowcast wire protocol.
//
// Transport: QUIC (RFC 9000) with TLS 1.3.
//
// Two channels over a single QUIC connection:
//
//  1. Stream 0 (reliable, ordered) — commands and configuration.
//     Each message is: [uint32 length][uint8 type][payload...]
//
//  2. Datagrams (unreliable, unordered) — audio, FFT, and telemetry.
//     Each datagram is: [uint8 type][payload...]
package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// --- Datagram types (unreliable channel) ---

const (
	// DatagramAudio carries an Opus-encoded audio frame.
	// Payload: raw Opus packet bytes.
	DatagramAudio byte = 0x01

	// DatagramFFT carries waterfall magnitude bins.
	// Payload: [uint16 numBins][uint8 bins...]
	// Each bin is a dBFS value mapped to 0-255.
	DatagramFFT byte = 0x02

	// DatagramStatus carries periodic telemetry.
	// Payload: [float32 smeter_dbm][float32 squelch_dbm][uint8 demod_mode]
	DatagramStatus byte = 0x03
)

// --- Command types (reliable stream) ---

const (
	// CmdSetFrequency sets the center frequency.
	// Payload: uint64 (frequency in Hz)
	CmdSetFrequency byte = 0x10

	// CmdSetMode sets the demodulation mode.
	// Payload: uint8 (DemodMode)
	CmdSetMode byte = 0x11

	// CmdSetSquelch sets the squelch level.
	// Payload: float32 (dBm)
	CmdSetSquelch byte = 0x12

	// CmdSetGain sets the tuner gain.
	// Payload: float32 (dB, or 0 for auto)
	CmdSetGain byte = 0x13

	// CmdStart begins streaming.
	// Payload: none.
	CmdStart byte = 0x20

	// CmdStop stops streaming.
	// Payload: none.
	CmdStop byte = 0x21

	// CmdHello is sent by the client on connect.
	// Payload: [uint8 protoVersion]
	CmdHello byte = 0x30

	// CmdWelcome is sent by the server in response to Hello.
	// Payload: [uint8 protoVersion][uint64 minFreqHz][uint64 maxFreqHz][uint32 sampleRate]
	CmdWelcome byte = 0x31

	// CmdAuth is sent by the client before Hello when connecting via relay.
	// Payload: [32 bytes SHA-256 hash of password]
	CmdAuth byte = 0x32

	// CmdAuthOK is sent by the relay to confirm successful auth.
	// Payload: none.
	CmdAuthOK byte = 0x33

	// CmdAuthFail is sent by the relay on bad password.
	// Payload: none.
	CmdAuthFail byte = 0x34
)

const ProtoVersion = 1

// DemodMode enumerates supported demodulation modes.
type DemodMode uint8

const (
	ModeNFM DemodMode = iota // Narrowband FM (12.5 kHz)
	ModeWFM                  // Wideband FM (200 kHz)
	ModeAM                   // AM (aviation band)
)

func (m DemodMode) String() string {
	switch m {
	case ModeNFM:
		return "NFM"
	case ModeWFM:
		return "WFM"
	case ModeAM:
		return "AM"
	default:
		return fmt.Sprintf("Unknown(%d)", m)
	}
}

// ChannelBandwidth returns the RF bandwidth for this mode in Hz.
func (m DemodMode) ChannelBandwidth() int {
	switch m {
	case ModeNFM:
		return 16_000
	case ModeWFM:
		return 200_000
	case ModeAM:
		return 10_000
	default:
		return 16_000
	}
}

// AudioRate returns the output audio sample rate for this mode.
func (m DemodMode) AudioRate() int {
	switch m {
	case ModeWFM:
		return 48000
	case ModeNFM, ModeAM:
		return 16000
	default:
		return 16000
	}
}

// --- Wire helpers ---

// WriteCmd writes a length-prefixed command to w.
func WriteCmd(w io.Writer, cmdType byte, payload []byte) error {
	// [uint32 length][uint8 type][payload]
	length := uint32(1 + len(payload))
	if err := binary.Write(w, binary.LittleEndian, length); err != nil {
		return err
	}
	if _, err := w.Write([]byte{cmdType}); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := w.Write(payload)
		return err
	}
	return nil
}

// ReadCmd reads a length-prefixed command from r.
func ReadCmd(r io.Reader) (cmdType byte, payload []byte, err error) {
	var length uint32
	if err = binary.Read(r, binary.LittleEndian, &length); err != nil {
		return
	}
	if length == 0 || length > 1024 {
		err = fmt.Errorf("invalid command length: %d", length)
		return
	}
	buf := make([]byte, length)
	if _, err = io.ReadFull(r, buf); err != nil {
		return
	}
	cmdType = buf[0]
	payload = buf[1:]
	return
}

// --- Payload encoding helpers ---

func EncodeUint64(v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b
}

func DecodeUint64(b []byte) uint64 {
	return binary.LittleEndian.Uint64(b)
}

func EncodeFloat32(v float32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, math.Float32bits(v))
	return b
}

func DecodeFloat32(b []byte) float32 {
	return math.Float32frombits(binary.LittleEndian.Uint32(b))
}
