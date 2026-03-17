package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"os/signal"
	"sync"
	"syscall"

	"github.com/pierr3/narrowcast/pkg/protocol"
	"github.com/quic-go/quic-go"
)

func main() {
	listen := flag.String("listen", "0.0.0.0:4444", "Public listen address (host:port)")
	upstream := flag.String("upstream", "", "Upstream narrowcast server address (host:port)")
	password := flag.String("password", "", "Required password for client auth")
	certFile := flag.String("cert", "certs/server.crt", "TLS certificate file")
	keyFile := flag.String("key", "certs/server.key", "TLS private key file")
	flag.Parse()

	if *upstream == "" {
		log.Fatal("--upstream is required (e.g., 192.168.1.50:4444)")
	}
	if *password == "" {
		log.Fatal("--password is required")
	}

	passwordHash := sha256.Sum256([]byte(*password))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, *listen, *upstream, passwordHash, *certFile, *keyFile); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run(ctx context.Context, listenAddr, upstreamAddr string, passwordHash [32]byte, certFile, keyFile string) error {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("load TLS cert: %w", err)
	}

	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"narrowcast-v1"},
	}
	quicConf := &quic.Config{
		EnableDatagrams: true,
		Allow0RTT:       true,
	}

	udpAddr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return fmt.Errorf("resolve listen addr: %w", err)
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("listen udp: %w", err)
	}
	tr := &quic.Transport{Conn: udpConn}
	ln, err := tr.Listen(tlsConf, quicConf)
	if err != nil {
		return fmt.Errorf("quic listen: %w", err)
	}
	defer ln.Close()

	log.Printf("[relay] listening on %s, upstream=%s", listenAddr, upstreamAddr)

	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go handleClient(ctx, conn, upstreamAddr, passwordHash)
	}
}

func handleClient(ctx context.Context, clientConn quic.Connection, upstreamAddr string, passwordHash [32]byte) {
	remote := clientConn.RemoteAddr().String()
	log.Printf("[relay] client connected: %s", remote)

	defer func() {
		clientConn.CloseWithError(0, "bye")
		log.Printf("[relay] client disconnected: %s", remote)
	}()

	// Step 1: Wait for CmdAuth datagram
	authCtx, authCancel := context.WithTimeout(ctx, 10_000_000_000) // 10s
	defer authCancel()

	dgram, err := clientConn.ReceiveDatagram(authCtx)
	if err != nil {
		log.Printf("[relay] %s: auth timeout: %v", remote, err)
		return
	}
	if len(dgram) < 33 || dgram[0] != protocol.CmdAuth {
		log.Printf("[relay] %s: expected CmdAuth, got type=0x%02x len=%d", remote, dgram[0], len(dgram))
		_ = clientConn.SendDatagram([]byte{protocol.CmdAuthFail})
		return
	}

	var clientHash [32]byte
	copy(clientHash[:], dgram[1:33])
	if clientHash != passwordHash {
		log.Printf("[relay] %s: auth failed (bad password)", remote)
		_ = clientConn.SendDatagram([]byte{protocol.CmdAuthFail})
		return
	}

	_ = clientConn.SendDatagram([]byte{protocol.CmdAuthOK})
	log.Printf("[relay] %s: authenticated", remote)

	// Step 2: Connect to upstream
	upstreamTLS := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"narrowcast-v1"},
	}
	upstreamQUIC := &quic.Config{
		EnableDatagrams: true,
	}

	upConn, err := quic.DialAddr(ctx, upstreamAddr, upstreamTLS, upstreamQUIC)
	if err != nil {
		log.Printf("[relay] %s: upstream connect failed: %v", remote, err)
		return
	}
	defer upConn.CloseWithError(0, "relay-done")
	log.Printf("[relay] %s: connected to upstream %s", remote, upstreamAddr)

	// Step 3: Bidirectional datagram forwarding
	var wg sync.WaitGroup
	relayCtx, relayCancel := context.WithCancel(ctx)
	defer relayCancel()

	// Client → Upstream
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer relayCancel()
		for {
			dg, err := clientConn.ReceiveDatagram(relayCtx)
			if err != nil {
				return
			}
			_ = upConn.SendDatagram(dg)
		}
	}()

	// Upstream → Client
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer relayCancel()
		for {
			dg, err := upConn.ReceiveDatagram(relayCtx)
			if err != nil {
				return
			}
			_ = clientConn.SendDatagram(dg)
		}
	}()

	wg.Wait()
	log.Printf("[relay] %s: session ended", remote)
}
