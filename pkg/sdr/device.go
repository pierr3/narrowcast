package sdr

// SDRDevice is the interface for both real and simulated SDR devices.
type SDRDevice interface {
	Close() error
	SetCenterFreq(hz uint64) error
	SetGain(auto bool, gainDB float64) error
	ReadAsync(cb func(buf []byte), bufCount, bufSize int) error
	CancelAsync() error
	GetSampleRate() int
	GetCenterFreq() uint64
}
