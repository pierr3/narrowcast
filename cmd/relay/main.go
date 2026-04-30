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
	"time"

	"github.com/pierr3/narrowcast/pkg/protocol"
	"github.com/quic-go/quic-go"
)

func main() {
	listen := flag.String("listen", "0.0.0.0:4444", "Public listen address")
	uplinkKey := flag.String("uplink-key", "", "Key for the Pi uplink to authenticate")
	password := flag.String("password", "", "Password for client auth")
	certFile := flag.String("cert", "certs/server.crt", "TLS certificate file")
	keyFile := flag.String("key", "certs/server.key", "TLS private key file")
	flag.Parse()

	if *uplinkKey == "" {
		log.Fatal("--uplink-key is required")
	}
	if *password == "" {
		log.Fatal("--password is required")
	}

	uplinkHash := sha256.Sum256([]byte(*uplinkKey))
	clientHash := sha256.Sum256([]byte(*password))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	r := &relay{
		uplinkHash: uplinkHash,
		clientHash: clientHash,
	}

	if err := r.run(ctx, *listen, *certFile, *keyFile); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

type relay struct {
	uplinkHash [32]byte
	clientHash [32]byte

	mu       sync.RWMutex
	upstream quic.Connection            // the Pi uplink connection
	clients  map[string]quic.Connection // connected clients
}

func (r *relay) run(ctx context.Context, listenAddr, certFile, keyFile string) error {
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
		// Symmetric keepalive on the listener side. quic-go's default
		// MaxIdleTimeout is 30 s; if the uplink's PINGs ever skip a beat
		// (NAT churn, brief network blip) the relay would drop. Sending
		// our own PING every 10 s + a 60 s idle ceiling means it takes
		// six consecutive missed PINGs to declare the link dead — far
		// more resilient than the baseline 30 s no-traffic guillotine.
		KeepAlivePeriod: 10 * time.Second,
		MaxIdleTimeout:  60 * time.Second,
	}

	udpAddr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return fmt.Errorf("resolve addr: %w", err)
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

	r.clients = make(map[string]quic.Connection)

	log.Printf("[relay] listening on %s (waiting for uplink + clients)", listenAddr)

	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go r.handleNewConnection(ctx, conn)
	}
}

func (r *relay) handleNewConnection(ctx context.Context, conn quic.Connection) {
	remote := conn.RemoteAddr().String()

	// First datagram determines if this is an uplink or a client
	authCtx, authCancel := context.WithTimeout(ctx, 10_000_000_000) // 10s
	defer authCancel()

	dgram, err := conn.ReceiveDatagram(authCtx)
	if err != nil {
		log.Printf("[relay] %s: first datagram timeout: %v", remote, err)
		conn.CloseWithError(0, "timeout")
		return
	}
	if len(dgram) < 1 {
		conn.CloseWithError(0, "empty")
		return
	}

	switch dgram[0] {
	case protocol.CmdUplink:
		r.handleUplink(ctx, conn, dgram, remote)
	case protocol.CmdAuth:
		r.handleClient(ctx, conn, dgram, remote)
	default:
		log.Printf("[relay] %s: unexpected first command 0x%02x", remote, dgram[0])
		conn.CloseWithError(0, "unexpected")
	}
}

func (r *relay) handleUplink(ctx context.Context, conn quic.Connection, dgram []byte, remote string) {
	if len(dgram) < 33 {
		log.Printf("[relay] %s: uplink auth too short", remote)
		conn.CloseWithError(0, "bad-auth")
		return
	}

	var hash [32]byte
	copy(hash[:], dgram[1:33])
	if hash != r.uplinkHash {
		log.Printf("[relay] %s: uplink auth failed", remote)
		_ = conn.SendDatagram([]byte{protocol.CmdAuthFail})
		conn.CloseWithError(0, "bad-auth")
		return
	}

	_ = conn.SendDatagram([]byte{protocol.CmdAuthOK})

	// Replace existing upstream
	r.mu.Lock()
	if r.upstream != nil {
		r.upstream.CloseWithError(0, "replaced")
	}
	r.upstream = conn
	r.mu.Unlock()

	log.Printf("[relay] uplink connected: %s", remote)

	// Read datagrams from upstream and fan out to all clients
	for {
		dg, err := conn.ReceiveDatagram(ctx)
		if err != nil {
			break
		}
		r.fanOutToClients(dg)
	}

	r.mu.Lock()
	if r.upstream == conn {
		r.upstream = nil
	}
	r.mu.Unlock()
	log.Printf("[relay] uplink disconnected: %s", remote)
}

func (r *relay) handleClient(ctx context.Context, conn quic.Connection, dgram []byte, remote string) {
	if len(dgram) < 33 {
		log.Printf("[relay] %s: client auth too short", remote)
		conn.CloseWithError(0, "bad-auth")
		return
	}

	var hash [32]byte
	copy(hash[:], dgram[1:33])
	if hash != r.clientHash {
		log.Printf("[relay] %s: client auth failed", remote)
		_ = conn.SendDatagram([]byte{protocol.CmdAuthFail})
		conn.CloseWithError(0, "bad-auth")
		return
	}

	_ = conn.SendDatagram([]byte{protocol.CmdAuthOK})

	// Register client
	r.mu.Lock()
	r.clients[remote] = conn
	r.mu.Unlock()

	log.Printf("[relay] client authenticated: %s (%d clients)", remote, len(r.clients))

	defer func() {
		r.mu.Lock()
		delete(r.clients, remote)
		remaining := len(r.clients)
		up := r.upstream
		r.mu.Unlock()
		conn.CloseWithError(0, "bye")
		log.Printf("[relay] client disconnected: %s (%d remaining)", remote, remaining)

		// Stop SDR when last client disconnects
		if remaining == 0 && up != nil {
			_ = up.SendDatagram([]byte{protocol.CmdStop})
			log.Printf("[relay] no clients remaining, sent Stop to upstream")
		}
	}()

	// Read datagrams from client and forward to upstream (all clients can tune)
	for {
		dg, err := conn.ReceiveDatagram(ctx)
		if err != nil {
			return
		}
		r.mu.RLock()
		up := r.upstream
		r.mu.RUnlock()
		if up != nil {
			_ = up.SendDatagram(dg)
		}
	}
}

func (r *relay) fanOutToClients(dg []byte) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Append client count to status datagrams so clients can display it
	out := dg
	if len(dg) > 0 && dg[0] == protocol.DatagramStatus {
		out = make([]byte, len(dg)+1)
		copy(out, dg)
		count := len(r.clients)
		if count > 255 {
			count = 255
		}
		out[len(dg)] = byte(count)
	}

	for _, c := range r.clients {
		_ = c.SendDatagram(out)
	}
}
