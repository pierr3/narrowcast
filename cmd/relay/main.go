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

// relay holds one upstream (the Pi uplink) plus N clients and moves datagrams
// between them.
//
// Every send goes through a protocol.Writer, which is load-bearing rather than
// tidiness: quic-go's SendDatagram blocks once 32 frames are queued, so the
// previous direct-send fan-out let one slow phone stall the loop that reads from
// upstream. That backpressure travelled the whole chain — relay stops reading →
// uplink's send blocks → uplink stops reading the Pi → the Pi's pipeline blocks
// → SDR buffers drop → DSP resets. A single bad client froze every listener and
// glitched the radio. Writers shed datagrams per connection instead.
type relay struct {
	uplinkHash [32]byte
	clientHash [32]byte

	mu           sync.RWMutex
	upstream     *protocol.Writer
	upstreamConn quic.Connection
	clients      map[string]*protocol.Writer
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

	r.clients = make(map[string]*protocol.Writer)

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
	authCtx, authCancel := context.WithTimeout(ctx, 10*time.Second)
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

// checkAuth compares the 32-byte hash in an auth datagram against want,
// replying and closing the connection on failure.
//
// Auth replies go out directly rather than through a Writer: the connection is
// brand new so its datagram queue is empty (no blocking risk), and the failure
// path needs the reply on the wire before CloseWithError.
func checkAuth(conn quic.Connection, dgram []byte, want [32]byte, role, remote string) bool {
	if len(dgram) < 33 {
		log.Printf("[relay] %s: %s auth too short", remote, role)
		conn.CloseWithError(0, "bad-auth")
		return false
	}
	var hash [32]byte
	copy(hash[:], dgram[1:33])
	if hash != want {
		log.Printf("[relay] %s: %s auth failed", remote, role)
		_ = conn.SendDatagram([]byte{protocol.CmdAuthFail})
		conn.CloseWithError(0, "bad-auth")
		return false
	}
	_ = conn.SendDatagram([]byte{protocol.CmdAuthOK})
	return true
}

func (r *relay) handleUplink(ctx context.Context, conn quic.Connection, dgram []byte, remote string) {
	if !checkAuth(conn, dgram, r.uplinkHash, "uplink", remote) {
		return
	}

	w := protocol.NewWriter(conn)

	// Replace any existing upstream.
	r.mu.Lock()
	if r.upstreamConn != nil {
		r.upstream.Close()
		r.upstreamConn.CloseWithError(0, "replaced")
	}
	r.upstream = w
	r.upstreamConn = conn
	r.mu.Unlock()

	log.Printf("[relay] uplink connected: %s", remote)

	// Read datagrams from upstream and fan out to all clients.
	for {
		dg, err := conn.ReceiveDatagram(ctx)
		if err != nil {
			break
		}
		r.fanOutToClients(dg)
	}

	r.mu.Lock()
	if r.upstreamConn == conn {
		r.upstream = nil
		r.upstreamConn = nil
	}
	r.mu.Unlock()
	w.Close()

	if drops, errs := w.Stats(); drops > 0 || errs > 0 {
		log.Printf("[relay] uplink %s: shed %d datagrams, %d send errors", remote, drops, errs)
	}
	log.Printf("[relay] uplink disconnected: %s", remote)
}

func (r *relay) handleClient(ctx context.Context, conn quic.Connection, dgram []byte, remote string) {
	if !checkAuth(conn, dgram, r.clientHash, "client", remote) {
		return
	}

	w := protocol.NewWriter(conn)

	r.mu.Lock()
	r.clients[remote] = w
	count := len(r.clients)
	r.mu.Unlock()

	log.Printf("[relay] client authenticated: %s (%d clients)", remote, count)

	defer func() {
		r.mu.Lock()
		delete(r.clients, remote)
		remaining := len(r.clients)
		up := r.upstream
		r.mu.Unlock()

		w.Close()
		conn.CloseWithError(0, "bye")
		if drops, errs := w.Stats(); drops > 0 || errs > 0 {
			log.Printf("[relay] client %s: shed %d datagrams, %d send errors", remote, drops, errs)
		}
		log.Printf("[relay] client disconnected: %s (%d remaining)", remote, remaining)

		// Stop the SDR when the last client disconnects.
		if remaining == 0 && up != nil {
			up.Send([]byte{protocol.CmdStop})
			log.Printf("[relay] no clients remaining, sent Stop to upstream")
		}
	}()

	// Read datagrams from the client and forward upstream (all clients can tune).
	for {
		dg, err := conn.ReceiveDatagram(ctx)
		if err != nil {
			return
		}
		r.mu.RLock()
		up := r.upstream
		r.mu.RUnlock()
		if up != nil {
			up.Send(dg)
		}
	}
}

func (r *relay) fanOutToClients(dg []byte) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Append the client count to status datagrams so clients can display it.
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

	// Holding the read lock across these sends is safe precisely because
	// Writer.Send queues or drops and never blocks on the network. Sharing
	// `out` between writers is fine too: none of them mutate it, and quic-go
	// copies the payload into each datagram frame.
	for _, w := range r.clients {
		w.Send(out)
	}
}
