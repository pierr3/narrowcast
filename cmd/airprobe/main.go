// Command airprobe measures a live narrowcast server on real signals.
//
// It answers "why does this sound bad" with numbers instead of opinions, which
// matters because the obvious theories have repeatedly been wrong: the audio
// spectrum split into speech frames and inter-syllable gap frames shows exactly
// where the noise sits and how far below the voice it is, and that is what says
// whether a filter change could help at all.
//
// Read-only by construction: it sends Hello and Start and nothing else, so the
// operator's frequency, mode, squelch and gain are untouched. Tuning commands
// are last-writer-wins, so a probe that retuned would change the radio for
// everyone listening.
//
//	go run ./cmd/airprobe radio.local:4444
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"time"

	"github.com/pierr3/narrowcast/pkg/protocol"
	"github.com/quic-go/quic-go"
	"gopkg.in/hraban/opus.v2"
)

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

func main() {
	addr := os.Args[1]
	secs := 40
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs+10)*time.Second)
	defer cancel()

	conn, err := quic.DialAddr(ctx, addr,
		&tls.Config{InsecureSkipVerify: true, NextProtos: []string{"narrowcast-v1"}},
		&quic.Config{EnableDatagrams: true})
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.CloseWithError(0, "done")

	_ = conn.SendDatagram([]byte{protocol.CmdHello, protocol.ProtoVersion})
	_ = conn.SendDatagram([]byte{protocol.CmdStart})

	var smeter []float64
	var squelchDb float64
	var freq uint64
	var mode byte = 255
	var opusPkts [][]byte
	var fftAccum []float64
	var fftFrames int

	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	for time.Now().Before(deadline) {
		d, err := conn.ReceiveDatagram(ctx)
		if err != nil {
			break
		}
		switch d[0] {
		case protocol.DatagramStatus:
			if len(d) >= 18 {
				smeter = append(smeter, float64(protocol.DecodeFloat32(d[1:])))
				squelchDb = float64(protocol.DecodeFloat32(d[5:]))
				mode = d[9]
				freq = protocol.DecodeUint64(d[10:])
			}
		case protocol.DatagramAudioSeq:
			if len(d) > 3 {
				opusPkts = append(opusPkts, append([]byte(nil), d[3:]...))
			}
		case protocol.DatagramAudio:
			if len(d) > 1 {
				opusPkts = append(opusPkts, append([]byte(nil), d[1:]...))
			}
		case protocol.DatagramFFT:
			if len(d) > 3 {
				bins := d[3:]
				if fftAccum == nil {
					fftAccum = make([]float64, len(bins))
				}
				if len(bins) == len(fftAccum) {
					for i, b := range bins {
						fftAccum[i] += float64(b)
					}
					fftFrames++
				}
			}
		}
	}

	modeName := map[byte]string{0: "NFM", 1: "WFM", 2: "AM"}[mode]
	rate := 16000
	if mode == 1 {
		rate = 48000
	}
	fmt.Printf("\n=== %s ===\n", addr)
	fmt.Printf("tuned %.4f MHz, mode %s, squelch %.1f dB\n", float64(freq)/1e6, modeName, squelchDb)

	// --- S-meter: how much of the reading is noise floor ---
	sort.Float64s(smeter)
	if len(smeter) > 10 {
		fmt.Printf("\nS-meter over %ds (%d frames), dB:\n", secs, len(smeter))
		fmt.Printf("  floor(p10) %7.1f   median %7.1f   p90 %7.1f   peak %7.1f\n",
			pct(smeter, 0.10), pct(smeter, 0.50), pct(smeter, 0.90), smeter[len(smeter)-1])
		fmt.Printf("  peak above floor: %.1f dB   squelch sits %.1f dB above floor\n",
			smeter[len(smeter)-1]-pct(smeter, 0.10), squelchDb-pct(smeter, 0.10))
	}

	// --- Audio: decode and measure per-frame level ---
	if len(opusPkts) == 0 {
		fmt.Printf("\nNo audio in %ds — squelch never opened, so nothing to measure.\n", secs)
		return
	}
	dec, err := opus.NewDecoder(rate, 1)
	if err != nil {
		log.Fatal(err)
	}
	frame := make([]int16, rate*20/1000)
	var levels []float64
	var pcm []float64
	for _, p := range opusPkts {
		n, err := dec.Decode(p, frame)
		if err != nil {
			continue
		}
		var sum float64
		for _, s := range frame[:n] {
			v := float64(s) / 32768
			sum += v * v
			pcm = append(pcm, v)
		}
		levels = append(levels, 10*math.Log10(sum/float64(n)+1e-20))
	}
	sortedLv := append([]float64(nil), levels...)
	sort.Float64s(sortedLv)
	fmt.Printf("\nAudio: %d frames (%.1f s of open squelch)\n", len(levels), float64(len(levels))*0.02)
	fmt.Printf("  quietest(p10) %6.1f dBFS   median %6.1f   loudest(p95) %6.1f\n",
		pct(sortedLv, 0.10), pct(sortedLv, 0.50), pct(sortedLv, 0.95))
	fmt.Printf("  speech-to-gap ratio: %.1f dB\n", pct(sortedLv, 0.95)-pct(sortedLv, 0.10))

	// --- Noise spectrum: average spectrum of the quietest 15% of frames ---
	// Those are inter-syllable gaps, held open by squelch hang, so they are the
	// hiss on its own.
	noiseThresh := pct(sortedLv, 0.15)
	voiceThresh := pct(sortedLv, 0.85)
	const nfft = 512
	noiseSpec := make([]float64, nfft/2)
	voiceSpec := make([]float64, nfft/2)
	var nNoise, nVoice int
	samplesPerFrame := rate * 20 / 1000
	for i, lv := range levels {
		start := i * samplesPerFrame
		if start+nfft > len(pcm) {
			break
		}
		buf := make([]complex128, nfft)
		for k := 0; k < nfft; k++ {
			w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(k)/float64(nfft-1))
			buf[k] = complex(pcm[start+k]*w, 0)
		}
		fftInPlace(buf)
		var dst []float64
		if lv <= noiseThresh {
			dst, nNoise = noiseSpec, nNoise+1
		} else if lv >= voiceThresh {
			dst, nVoice = voiceSpec, nVoice+1
		} else {
			continue
		}
		for k := 0; k < nfft/2; k++ {
			re, im := real(buf[k]), imag(buf[k])
			dst[k] += re*re + im*im
		}
	}
	if nNoise > 0 && nVoice > 0 {
		fmt.Printf("\nAudio spectrum, %d gap frames vs %d speech frames (dB, relative):\n", nNoise, nVoice)
		binHz := float64(rate) / nfft
		fmt.Printf("   freq     gaps   speech   speech-gaps\n")
		for _, hz := range []float64{200, 400, 700, 1000, 1500, 2000, 2500, 3000, 3500, 4000, 5000} {
			k := int(hz / binHz)
			if k >= nfft/2 {
				continue
			}
			n := 10 * math.Log10(noiseSpec[k]/float64(nNoise)+1e-20)
			v := 10 * math.Log10(voiceSpec[k]/float64(nVoice)+1e-20)
			fmt.Printf("  %5.0f Hz %7.1f %8.1f %10.1f\n", hz, n, v, v-n)
		}
	}

	// --- RF spectrum around the tuned channel ---
	if fftFrames > 0 {
		c := len(fftAccum) / 2
		fmt.Printf("\nRF spectrum, mean bin value near centre (%d frames, 255=0 dBFS over 120 dB):\n", fftFrames)
		fmt.Printf("  ")
		for off := -6; off <= 6; off++ {
			fmt.Printf("%5.0f", fftAccum[c+off]/float64(fftFrames))
		}
		fmt.Printf("\n  (centre bin is index 0 of that row's middle; each bin ~3.75 kHz)\n")
		var sum float64
		for _, v := range fftAccum {
			sum += v / float64(fftFrames)
		}
		fmt.Printf("  whole-span mean %.0f, centre %.0f, centre-minus-mean %.0f counts (%.1f dB)\n",
			sum/float64(len(fftAccum)), fftAccum[c]/float64(fftFrames),
			fftAccum[c]/float64(fftFrames)-sum/float64(len(fftAccum)),
			(fftAccum[c]/float64(fftFrames)-sum/float64(len(fftAccum)))/2.125)
	}
}

func fftInPlace(a []complex128) {
	n := len(a)
	if n <= 1 {
		return
	}
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	for length := 2; length <= n; length <<= 1 {
		ang := -2 * math.Pi / float64(length)
		wl := complex(math.Cos(ang), math.Sin(ang))
		for i := 0; i < n; i += length {
			w := complex(1, 0)
			for k := 0; k < length/2; k++ {
				u := a[i+k]
				v := a[i+k+length/2] * w
				a[i+k] = u + v
				a[i+k+length/2] = u - v
				w *= wl
			}
		}
	}
}
