package protocol

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// ClientHandler is called for each connected client.
// conn is the QUIC connection for sending/receiving datagrams.
// The handler should block until the client disconnects.
type ClientHandler func(ctx context.Context, conn quic.Connection)

// Server listens for QUIC connections and dispatches clients.
type Server struct {
	listener *quic.Listener
	handler  ClientHandler
	mu       sync.Mutex
	clients  map[string]quic.Connection
}

// NewServer creates a QUIC server.
func NewServer(addr string, certFile, keyFile string, handler ClientHandler) (*Server, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS cert: %w", err)
	}

	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"narrowcast-v1"},
	}

	quicConf := &quic.Config{
		EnableDatagrams: true,
		Allow0RTT:       true,
		// Symmetric keepalive on the SDR-side QUIC listener. The uplink
		// already sets KeepAlivePeriod on its dial; without the same on
		// the listener, quic-go negotiates to whichever side has the
		// shorter MaxIdleTimeout (default 30 s) and the link drops every
		// 30 s — exactly the pattern the relay log was showing. 10 s
		// PING / 60 s ceiling matches relay + uplink.
		KeepAlivePeriod: 10 * time.Second,
		MaxIdleTimeout:  60 * time.Second,
	}

	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolve addr: %w", err)
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("listen udp: %w", err)
	}

	tr := &quic.Transport{Conn: udpConn}
	ln, err := tr.Listen(tlsConf, quicConf)
	if err != nil {
		return nil, fmt.Errorf("quic listen: %w", err)
	}

	return &Server{
		listener: ln,
		handler:  handler,
		clients:  make(map[string]quic.Connection),
	}, nil
}

// Serve accepts connections until the context is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	log.Printf("[server] listening on %s", s.listener.Addr())

	for {
		conn, err := s.listener.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}

		go s.handleConn(ctx, conn)
	}
}

func (s *Server) handleConn(ctx context.Context, conn quic.Connection) {
	remote := conn.RemoteAddr().String()
	log.Printf("[server] client connected: %s", remote)

	s.mu.Lock()
	s.clients[remote] = conn
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.clients, remote)
		s.mu.Unlock()
		conn.CloseWithError(0, "bye")
		log.Printf("[server] client disconnected: %s", remote)
	}()

	s.handler(ctx, conn)
}

// Close shuts down the server.
func (s *Server) Close() error {
	return s.listener.Close()
}
