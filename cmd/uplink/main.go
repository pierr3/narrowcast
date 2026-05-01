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
	"net"
	"os/signal"
	"strconv"
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

	// Resolve the relay hostname ONCE at startup. The Pi's resolver
	// (systemd-resolved) flakes during network blips and any reconnect
	// triggered while it's recovering would wait the full DNS timeout
	// per attempt. We cache the IP for the lifetime of the process so
	// later reconnects are pure-IP dials. TLS cert validation is
	// preserved by passing the original hostname as ServerName.
	relayHost, relayPort, err := splitHostPort(*relayAddr)
	if err != nil {
		log.Fatalf("--relay must be host:port: %v", err)
	}
	relayUDP, err := resolveRelayWithFallback(ctx, relayHost, relayPort)
	if err != nil {
		log.Fatalf("could not resolve relay %s: %v", *relayAddr, err)
	}
	log.Printf("[uplink] relay %s resolved to %s (cached for process lifetime)", *relayAddr, relayUDP)

	// Reconnect loop
	for {
		err := run(ctx, *local, relayUDP, relayHost, *uplinkKey)
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

// splitHostPort accepts "host:port" and returns parsed parts. Pulled out so
// the result can be reused for both the resolve step and the TLS ServerName.
func splitHostPort(addr string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("port: %w", err)
	}
	return host, port, nil
}

// resolveRelayWithFallback tries the system resolver first, then falls back
// to public DNS (Cloudflare) if the system resolver is misbehaving. This
// matters at startup on the Pi where systemd-resolved can be wedged and
// returning SERVFAIL even though the upstream DNS is fine. Retries with
// exponential backoff so network flakiness at boot doesn't kill the unit.
func resolveRelayWithFallback(ctx context.Context, host string, port int) (*net.UDPAddr, error) {
	publicResolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, network, "1.1.1.1:53")
		},
	}

	backoff := 1 * time.Second
	for {
		// System resolver first — usually fine and respects local
		// configuration like /etc/hosts overrides.
		if ip, err := lookupOne(ctx, net.DefaultResolver, host); err == nil {
			return &net.UDPAddr{IP: ip, Port: port}, nil
		} else {
			log.Printf("[uplink] system resolver failed for %s: %v — trying public DNS", host, err)
		}
		// Fallback: public DNS, bypassing systemd-resolved entirely.
		if ip, err := lookupOne(ctx, publicResolver, host); err == nil {
			log.Printf("[uplink] resolved %s via fallback resolver", host)
			return &net.UDPAddr{IP: ip, Port: port}, nil
		} else {
			log.Printf("[uplink] public resolver also failed for %s: %v — retrying in %v", host, err, backoff)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func lookupOne(ctx context.Context, r *net.Resolver, host string) (net.IP, error) {
	addrs, err := r.LookupHost(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no addresses for %s", host)
	}
	// Prefer the first IPv4 if one is available — IPv6-only paths to
	// VPSes have caused unrelated MTU issues for some users on consumer
	// ISPs. Fall back to whatever's first if no v4 is in the set.
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip != nil && ip.To4() != nil {
			return ip.To4(), nil
		}
	}
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil {
			return ip, nil
		}
	}
	return nil, fmt.Errorf("no parseable IP for %s", host)
}

func run(ctx context.Context, localAddr string, relayUDP *net.UDPAddr, relayHost, uplinkKey string) error {
	quicConf := &quic.Config{
		EnableDatagrams: true,
		// PING every 10 s, idle ceiling 60 s — symmetric with relay +
		// narrowcast listeners. Keeps NAT bindings warm and absorbs
		// brief network blips before they tear the link down.
		KeepAlivePeriod: 10 * time.Second,
		MaxIdleTimeout:  60 * time.Second,
	}

	// Build a UDP transport bound to an ephemeral local port. Reused
	// across both Dial calls in this run() invocation; closed when run()
	// returns so a fresh socket is allocated next reconnect (avoids
	// stuck NAT mappings during prolonged network outages).
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return fmt.Errorf("listen udp: %w", err)
	}
	tr := &quic.Transport{Conn: udpConn}
	defer tr.Close()

	// Relay connection: dial cached IP, validate cert against the
	// original hostname via ServerName. ServerName is what TLS uses for
	// SNI and certificate verification, so the LE cert for the relay
	// domain still matches even though we connected by IP.
	relayTLS := &tls.Config{
		NextProtos: []string{"narrowcast-v1"},
		ServerName: relayHost,
	}
	var relayConn quic.Connection
	backoff := 1 * time.Second
	for {
		dialCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		c, derr := tr.Dial(dialCtx, relayUDP, relayTLS, quicConf)
		cancel()
		if derr == nil {
			relayConn = c
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Printf("[uplink] relay dial failed: %v — retrying in %v", derr, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
	defer relayConn.CloseWithError(0, "uplink-done")
	log.Printf("[uplink] connected to relay at %s", relayUDP)

	// Local connection: self-signed Pi cert, skip verification.
	localTLS := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"narrowcast-v1"},
	}
	localConn, err := quic.DialAddr(ctx, localAddr, localTLS, quicConf)
	if err != nil {
		return fmt.Errorf("local connect: %w", err)
	}
	defer localConn.CloseWithError(0, "uplink-done")
	log.Printf("[uplink] connected to local narrowcast at %s", localAddr)

	// Authenticate with relay
	keyHash := sha256.Sum256([]byte(uplinkKey))
	authMsg := make([]byte, 33)
	authMsg[0] = protocol.CmdUplink
	copy(authMsg[1:], keyHash[:])
	if err := relayConn.SendDatagram(authMsg); err != nil {
		return fmt.Errorf("send uplink auth: %w", err)
	}

	authResp, err := relayConn.ReceiveDatagram(ctx)
	if err != nil {
		return fmt.Errorf("auth response: %w", err)
	}
	if len(authResp) < 1 || authResp[0] != protocol.CmdAuthOK {
		return fmt.Errorf("relay auth failed")
	}
	log.Printf("[uplink] authenticated with relay")

	// Send Hello to local server (establishes connection, but does NOT start streaming)
	hello := []byte{protocol.CmdHello, protocol.ProtoVersion}
	if err := localConn.SendDatagram(hello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	log.Printf("[uplink] bridging datagrams (SDR idle until client sends Start)")

	// Bidirectional forwarding
	var wg sync.WaitGroup
	bridgeCtx, bridgeCancel := context.WithCancel(ctx)
	defer bridgeCancel()

	// Capture which side died first so the parent log line can tell us
	// whether the local narrowcast dropped us or the public relay did.
	var localErr, relayErr error
	var errMu sync.Mutex

	// Local (Pi narrowcast) → Relay (fan-out to clients)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer bridgeCancel()
		for {
			dg, err := localConn.ReceiveDatagram(bridgeCtx)
			if err != nil {
				errMu.Lock()
				if localErr == nil {
					localErr = err
				}
				errMu.Unlock()
				return
			}
			_ = relayConn.SendDatagram(dg)
		}
	}()

	// Relay (client commands) → Local (Pi narrowcast)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer bridgeCancel()
		for {
			dg, err := relayConn.ReceiveDatagram(bridgeCtx)
			if err != nil {
				errMu.Lock()
				if relayErr == nil {
					relayErr = err
				}
				errMu.Unlock()
				return
			}
			_ = localConn.SendDatagram(dg)
		}
	}()

	wg.Wait()
	errMu.Lock()
	defer errMu.Unlock()
	switch {
	case localErr != nil && relayErr == nil:
		return fmt.Errorf("local narrowcast dropped: %w", localErr)
	case relayErr != nil && localErr == nil:
		return fmt.Errorf("relay dropped: %w", relayErr)
	case localErr != nil && relayErr != nil:
		return fmt.Errorf("local: %v, relay: %v", localErr, relayErr)
	default:
		return fmt.Errorf("bridge closed (no recorded error)")
	}
}
