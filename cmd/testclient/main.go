package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"time"

	"github.com/pierr3/narrowcast/pkg/protocol"
	"github.com/quic-go/quic-go"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"narrowcast-v1"},
	}
	quicConf := &quic.Config{
		EnableDatagrams: true,
	}

	log.Println("Connecting to localhost:4444...")
	conn, err := quic.DialAddr(ctx, "localhost:4444", tlsConf, quicConf)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.CloseWithError(0, "done")
	log.Println("Connected!")

	// Send Hello datagram
	hello := []byte{protocol.CmdHello, protocol.ProtoVersion}
	if err := conn.SendDatagram(hello); err != nil {
		log.Fatalf("send hello: %v", err)
	}
	log.Println("Sent Hello")

	// Receive Welcome
	dgram, err := conn.ReceiveDatagram(ctx)
	if err != nil {
		log.Fatalf("recv welcome: %v", err)
	}
	if len(dgram) > 0 && dgram[0] == protocol.CmdWelcome {
		log.Printf("Got Welcome! len=%d", len(dgram))
		if len(dgram) >= 22 {
			minFreq := protocol.DecodeUint64(dgram[2:])
			maxFreq := protocol.DecodeUint64(dgram[10:])
			sampleRate := protocol.DecodeFloat32(dgram[18:])
			log.Printf("  minFreq=%d maxFreq=%d sampleRate=%.0f", minFreq, maxFreq, sampleRate)
		}
	} else {
		log.Printf("Unexpected first datagram: type=0x%02x len=%d", dgram[0], len(dgram))
	}

	// Send Start streaming
	if err := conn.SendDatagram([]byte{protocol.CmdStart}); err != nil {
		log.Fatalf("send start: %v", err)
	}
	log.Println("Sent Start")

	// Receive some data datagrams
	audioPkts := 0
	fftPkts := 0
	statusPkts := 0
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			log.Printf("Done. audio=%d fft=%d status=%d", audioPkts, fftPkts, statusPkts)

			// Send Stop
			_ = conn.SendDatagram([]byte{protocol.CmdStop})
			log.Println("Sent Stop")
			time.Sleep(500 * time.Millisecond)
			return
		default:
			dg, err := conn.ReceiveDatagram(ctx)
			if err != nil {
				log.Printf("recv err: %v", err)
				return
			}
			if len(dg) == 0 {
				continue
			}
			switch dg[0] {
			case protocol.DatagramAudio, protocol.DatagramAudioSeq:
				audioPkts++
				if audioPkts == 1 {
					log.Printf("First audio datagram: %d bytes (type 0x%02x)", len(dg), dg[0])
				}
			case protocol.DatagramFFT:
				fftPkts++
				if fftPkts == 1 {
					log.Printf("First FFT datagram: %d bytes", len(dg))
				}
			case protocol.DatagramStatus:
				statusPkts++
			default:
				fmt.Printf("Unknown type: 0x%02x len=%d\n", dg[0], len(dg))
			}
		}
	}
}
