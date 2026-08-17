package main

// Shared-port demultiplexing: one TLS listener serves BOTH the relay (mux
// frames) and the web console (HTTP over the same TLS). After the TLS
// handshake we peek the first application bytes: if they look like an HTTP
// request line the connection is handed to the console; otherwise it is relayed.

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"syscall"
	"time"

	"deepseekaiworker/internal/admin"
)

// channelListener hands accepted conns to an http.Server over a channel.
type channelListener struct {
	ch     chan net.Conn
	addr   net.Addr
	closed chan struct{}
}

func (l *channelListener) Accept() (net.Conn, error) {
	select {
	case c, ok := <-l.ch:
		if !ok {
			return nil, io.EOF
		}
		return c, nil
	case <-l.closed:
		return nil, io.EOF
	}
}
func (l *channelListener) Close() error   { return nil }
func (l *channelListener) Addr() net.Addr { return l.addr }

// close signals the Accept loop (if any) to stop; primarily to release the
// console http.Server waiting on Accept during restart.
func (l *channelListener) close() {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
}

// bufferedConn wraps a TLS conn whose first bytes have been buffered (peeked),
// so downstream readers still see the full prefix.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// ResetRead pointed-to fields are immutable; nothing more needed.

// sharedListener drives a single raw TCP listener, terminating TLS and routing
// each connection to either the console (HTTP) or the relay handler.
type sharedListener struct {
	raw     net.Listener
	tlsCfg  *tls.Config
	console *channelListener
	rt      *runtime
	relayOK bool // when true, non-HTTP conns are relayed
	ctx     context.Context
}

func (s *sharedListener) loop() {
	for {
		c, err := s.raw.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			log.Printf("shared accept: %v", err)
			continue
		}
		go s.route(c)
	}
}

// maxPendingHandshakes bounds how many TLS handshakes run at once, so a flood
// of half-open/incomplete connections can't exhaust cgroups/fd/goroutine limits.
var handshakeSem = make(chan struct{}, maxPendingHandshakes)

const maxPendingHandshakes = 256

func (s *sharedListener) route(conn net.Conn) {
	// acquire a handshake slot; drop the connection if saturated
	select {
	case handshakeSem <- struct{}{}:
	case <-s.ctx.Done():
		conn.Close()
		return
	}
	tc := tls.Server(conn, s.tlsCfg)
	tc.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tc.Handshake(); err != nil {
		tc.Close()
		<-handshakeSem
		return
	}
	<-handshakeSem // handshake done; free the slot before routing

	br := bufio.NewReaderSize(tc, 32*1024)
	// Distinguish HTTP vs relay by the FIRST application byte: HTTP requests
	// begin with an ASCII method letter (G/P/H/O/D/T/C/U); the relay's first
	// frame is REGISTER (binary type byte 0x05, never a letter).
	peek, _ := br.Peek(1)
	tc.SetDeadline(time.Time{})
	bc := &bufferedConn{Conn: tc, r: br}
	if len(peek) > 0 && isASCILetter(peek[0]) {
		// hand off to the console HTTP server (it owns/closes the conn)
		select {
		case s.console.ch <- bc:
		case <-s.ctx.Done():
			tc.Close()
		}
		return
	}
	if !s.relayOK {
		tc.Close() // init mode / no relay: drop non-HTTP conns
		return
	}
	// hand off to the relay handler (it owns/closes the conn)
	go handleConnection(bc, s.rt)
}

// isASCILetter reports whether b is an ASCII letter (HTTP method start).
func isASCILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// startShared sets up the shared console+relay on a single TLS port.
func startShared(ctx context.Context, listenAddr string, tlsCfg *tls.Config, rt *runtime, relayOK bool, adm *admin.Server) (addr string, err error) {
	raw, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return "", err
	}
	cl := &channelListener{ch: make(chan net.Conn, 64), addr: raw.Addr(), closed: make(chan struct{})}
	registerStop(func() { raw.Close(); cl.close() })
	sl := &sharedListener{raw: raw, tlsCfg: tlsCfg, console: cl, rt: rt, relayOK: relayOK, ctx: ctx}
	go sl.loop()

	go func() {
		<-ctx.Done()
		raw.Close()
	}()

	// run the console http server on the console channel
	addr = raw.Addr().String()
	go func() {
		if err := adm.RunTLSOn(ctx, cl); err != nil && ctx.Err() == nil {
			log.Printf("web console (shared): %v", err)
		}
	}()
	return addr, nil
}

// runIndependent starts the console on its own listener (TLS if certs given).
func runIndependent(ctx context.Context, adminListen, certFile, keyFile string, adm *admin.Server) error {
	if keyFile != "" {
		return adm.RunTLS(ctx, adminListen, certFile, keyFile)
	}
	ln, err := listenOrEphemeral(adminListen)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); ln.Close() }()
	return adm.RunPlainOn(ctx, ln)
}

// listenOrEphemeral binds addr; if the address is already in use it falls back
// to an ephemeral free port (used for init-mode web console when 8443 is busy).
func listenOrEphemeral(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln, nil
	}
	if !isAddrInUse(err) {
		return nil, err
	}
	fallback, ferr := net.Listen("tcp", "127.0.0.1:0")
	if ferr != nil {
		return nil, ferr
	}
	log.Printf("WARNING: %s already in use; using ephemeral port %d for web console", addr, fallback.Addr().(*net.TCPAddr).Port)
	return fallback, nil
}

func isAddrInUse(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) {
		var sys *os.SyscallError
		if errors.As(err, &sys) {
			return errors.Is(sys.Err, syscall.EADDRINUSE)
		}
		return errors.Is(op.Err, syscall.EADDRINUSE)
	}
	return false
}
