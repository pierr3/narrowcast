// Uplink connects to a local Narrowcast server and a remote relay,
// bridging datagrams between them. Run this on the Pi alongside narrowcast.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/pierr3/narrowcast/pkg/protocol"
	"github.com/quic-go/quic-go"
)

func main() {
	local := flag.String("local", "localhost:4444", "Local narrowcast server address")
	relayAddr := flag.String("relay", "", "Remote relay server address (host:port)")
	uplinkKey := flag.String("key", "", "Uplink authentication key")
	flag.Parse()

	if *relayAddr == "" {
		log.Fatal("--relay is required (e.g., vps.example.com:4444)")
	}
	if *uplinkKey == "" {
		log.Fatal("--key is required")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Reconnect loop
	for {
		err := run(ctx, *local, *relayAddr, *uplinkKey)
		if ctx.Err() != nil {
			return
		}
		log.Printf("[uplink] connection lost: %v — reconnecting in 3s", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func run(ctx context.Context, localAddr, relayAddr, uplinkKey string) error {
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"narrowcast-v1"},
	}
	quicConf := &quic.Config{
		EnableDatagrams: true,
	}

	// Connect to local narrowcast server
	localConn, err := quic.DialAddr(ctx, localAddr, tlsConf, quicConf)
	if err != nil {
		return fmt.Errorf("local connect: %w", err)
	}
	defer localConn.CloseWithError(0, "uplink-done")
	log.Printf("[uplink] connected to local narrowcast at %s", localAddr)

	// Connect to remote relay
	relayConn, err := quic.DialAddr(ctx, relayAddr, tlsConf, quicConf)
	if err != nil {
		return fmt.Errorf("relay connect: %w", err)
	}
	defer relayConn.CloseWithError(0, "uplink-done")
	log.Printf("[uplink] connected to relay at %s", relayAddr)

	// Authenticate with relay
	keyHash := sha256.Sum256([]byte(uplinkKey))
	authMsg := make([]byte, 33)
	authMsg[0] = protocol.CmdUplink
	copy(authMsg[1:], keyHash[:])
	if err := relayConn.SendDatagram(authMsg); err != nil {
		return fmt.Errorf("send uplink auth: %w", err)
	}

	// Wait for auth response
	authResp, err := relayConn.ReceiveDatagram(ctx)
	if err != nil {
		return fmt.Errorf("auth response: %w", err)
	}
	if len(authResp) < 1 || authResp[0] != protocol.CmdAuthOK {
		return fmt.Errorf("relay auth failed")
	}
	log.Printf("[uplink] authenticated with relay")

	// Send Hello to local server to initiate connection
	hello := []byte{protocol.CmdHello, protocol.ProtoVersion}
	if err := localConn.SendDatagram(hello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	// Start streaming from local server
	if err := localConn.SendDatagram([]byte{protocol.CmdStart}); err != nil {
		return fmt.Errorf("send start: %w", err)
	}
	log.Printf("[uplink] streaming started, bridging datagrams")

	// Bidirectional forwarding
	var wg sync.WaitGroup
	bridgeCtx, bridgeCancel := context.WithCancel(ctx)
	defer bridgeCancel()

	// Local (Pi) → Relay (for fan-out to clients)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer bridgeCancel()
		for {
			dg, err := localConn.ReceiveDatagram(bridgeCtx)
			if err != nil {
				return
			}
			_ = relayConn.SendDatagram(dg)
		}
	}()

	// Relay (client commands) → Local (Pi)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer bridgeCancel()
		for {
			dg, err := relayConn.ReceiveDatagram(bridgeCtx)
			if err != nil {
				return
			}
			_ = localConn.SendDatagram(dg)
		}
	}()

	wg.Wait()
	return fmt.Errorf("bridge closed")
}
