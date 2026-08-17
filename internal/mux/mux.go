// Package mux implements a simple length-framed, multiplexed stream protocol
// that runs over a single persistent net.Conn (e.g. a TLS connection). It is
// shared by the client and server binaries.
//
// Frame layout (all integers big-endian):
//
//	+------+----------------+----------------+---------+
//	| type |  connection id |  payload len   | payload |
//	| 1B   |  4 bytes       |  4 bytes       |  n      |
//	+------+----------------+----------------+---------+
//
// Frame types:
//
//	0x01 OPEN   client -> server. Carries the local listen port (2 bytes) as
//	            the payload of the first frame of a new stream. Establishes a
//	            stream with the given connection id.
//	0x02 DATA   either direction. Raw tunneled payload bytes.
//	0x03 CLOSE  either direction. Half-close: the sender will send no more
//	            DATA on this stream; the peer sees io.EOF after draining.
//	0x04 CONFIG server -> client. Payload is a JSON Config document (see
//	            config.go). Delivered once after the handshake.
//	0x05 REGISTER client -> server. Payload is a JSON Register document (see
//	            config.go) identifying role/identity/services. Sent by every
//	            client as the first frame after the TLS handshake.
package mux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// Frame types
const (
	TypeOpen     byte = 0x01
	TypeData     byte = 0x02
	TypeClose    byte = 0x03
	TypeConfig   byte = 0x04
	TypeRegister byte = 0x05
)

const (
	headerLen  = 9
	maxPayload = 1 << 20 // 1 MiB per frame
)

// Mux multiplexes many logical streams over one underlying connection.
type Mux struct {
	conn   net.Conn
	sendMu sync.Mutex // serializes frame writes

	mu      sync.Mutex
	streams map[uint32]*Stream
	nextID  uint32
	config  chan []byte // receives one CONFIG payload, then closed
	reg     chan []byte // receives one REGISTER payload, then closed

	// OnOpen, when non-nil, is invoked for an incoming OPEN frame with the
	// listen port carried in its payload. The server uses this to establish
	// the mirrored stream and dial its target.
	OnOpen   func(s *Stream, listenPort uint16)
	onOpenMu sync.Mutex

	dead   chan struct{}
	deadMu sync.Once
}

// SetOnOpen registers the OPEN handler. Called by the server before the
// client can possibly open a stream.
func (m *Mux) SetOnOpen(fn func(s *Stream, listenPort uint16)) {
	m.onOpenMu.Lock()
	m.OnOpen = fn
	m.onOpenMu.Unlock()
}

// Dial opens a Mux over an already-connected net.Conn and starts reading it.
func Dial(conn net.Conn) *Mux {
	m := &Mux{
		conn:    conn,
		streams: make(map[uint32]*Stream),
		nextID:  1,
		config:  make(chan []byte, 1),
		reg:     make(chan []byte, 1),
		dead:    make(chan struct{}),
	}
	go m.readLoop()
	return m
}

// Register returns the REGISTER frame payload (waits for it if needed).
func (m *Mux) Register() ([]byte, error) {
	r, ok := <-m.reg
	if !ok {
		return nil, errors.New("connection closed before register received")
	}
	return r, nil
}

// SendRegister transmits the REGISTER frame (client side).
func (m *Mux) SendRegister(body []byte) error { return m.send(TypeRegister, 0, body) }

// Config returns the CONFIG frame payload (waits for it if needed).
func (m *Mux) Config() ([]byte, error) {
	cfg, ok := <-m.config
	if !ok {
		return nil, errors.New("connection closed before config received")
	}
	return cfg, nil
}

// Open creates a new client-side stream tagged with a listen port. On the
// server side the corresponding OPEN frame is delivered to OnOpen.
func (m *Mux) Open(port uint16) (*Stream, error) {
	m.mu.Lock()
	id := m.nextID
	m.nextID++
	if id == 0 {
		id++
		m.nextID++
	}
	s := newStream(m, id)
	m.streams[id] = s
	m.mu.Unlock()

	if err := m.send(TypeOpen, id, []byte{byte(port >> 8), byte(port)}); err != nil {
		m.remove(id)
		s.markDead(err)
		return s, err
	}
	return s, nil
}

func (m *Mux) newStream(id uint32) *Stream {
	s := newStream(m, id)
	m.mu.Lock()
	m.streams[id] = s
	m.mu.Unlock()
	return s
}

func (m *Mux) remove(id uint32) {
	m.mu.Lock()
	delete(m.streams, id)
	m.mu.Unlock()
}

func (m *Mux) send(typ byte, id uint32, payload []byte) error {
	if len(payload) > maxPayload {
		return fmt.Errorf("payload too large: %d", len(payload))
	}
	var buf [headerLen]byte
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], id)
	binary.BigEndian.PutUint32(buf[5:9], uint32(len(payload)))

	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	if _, err := m.conn.Write(buf[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := m.conn.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func (m *Mux) readLoop() {
	defer close(m.config)
	defer close(m.reg)
	defer m.terminateAll()
	head := make([]byte, headerLen)
	for {
		if _, err := io.ReadFull(m.conn, head); err != nil {
			return
		}
		typ := head[0]
		id := binary.BigEndian.Uint32(head[1:5])
		n := int(binary.BigEndian.Uint32(head[5:9]))
		if n > maxPayload {
			return
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(m.conn, body); err != nil {
			return
		}
		switch typ {
		case TypeData:
			m.mu.Lock()
			s := m.streams[id]
			m.mu.Unlock()
			if s != nil {
				s.push(body)
			}
		case TypeClose:
			m.mu.Lock()
			s := m.streams[id]
			m.mu.Unlock()
			if s != nil {
				s.remoteClose()
			}
		case TypeConfig:
			select {
			case m.config <- body:
			default:
			}
		case TypeRegister:
			select {
			case m.reg <- body:
			default:
			}
		case TypeOpen:
			m.onOpenMu.Lock()
			fn := m.OnOpen
			m.onOpenMu.Unlock()
			if fn != nil {
				port := uint16(0)
				if len(body) >= 2 {
					port = binary.BigEndian.Uint16(body[:2])
				}
				s := m.newStream(id)
				fn(s, port)
			}
		}
	}
}

// terminateAll tears down every stream when the underlying conn dies.
func (m *Mux) terminateAll() {
	m.markDead()
	m.mu.Lock()
	streams := make([]*Stream, 0, len(m.streams))
	for _, s := range m.streams {
		streams = append(streams, s)
	}
	m.streams = map[uint32]*Stream{}
	m.mu.Unlock()
	for _, s := range streams {
		s.terminate(errors.New("connection closed"))
	}
}

// IsDead reports whether the underlying connection has failed.
func (m *Mux) IsDead() bool {
	select {
	case <-m.dead:
		return true
	default:
		return false
	}
}

// DeadChan returns a channel closed when the underlying connection fails.
func (m *Mux) DeadChan() <-chan struct{} { return m.dead }

func (m *Mux) markDead() {
	m.deadMu.Do(func() { close(m.dead) })
}

// SendConfig transmits the CONFIG frame (server side).
func (m *Mux) SendConfig(body []byte) error { return m.send(TypeConfig, 0, body) }

// Stream is one logical bidirectional half-close-capable byte stream.
type Stream struct {
	m  *Mux
	id uint32

	// reader state
	rmu     sync.Mutex
	rcond   *sync.Cond
	buffer  []byte
	readEOF bool // remote sent CLOSE (or mux died)
	readErr error

	// writer state
	wmu       sync.Mutex
	closeSent bool
}

func newStream(m *Mux, id uint32) *Stream {
	s := &Stream{m: m, id: id}
	s.rcond = sync.NewCond(&s.rmu)
	return s
}

func (s *Stream) ID() uint32 { return s.id }

func (s *Stream) push(data []byte) {
	s.rmu.Lock()
	s.buffer = append(s.buffer, data...)
	s.rcond.Broadcast()
	s.rmu.Unlock()
}

// Read blocks until data is available, or returns io.EOF once the remote has
// half-closed (buffered data is drained first).
func (s *Stream) Read(p []byte) (int, error) {
	s.rmu.Lock()
	for len(s.buffer) == 0 && !s.readEOF && s.readErr == nil {
		s.rcond.Wait()
	}
	if len(s.buffer) > 0 {
		n := copy(p, s.buffer)
		s.buffer = s.buffer[n:]
		if len(s.buffer) == 0 {
			s.buffer = nil
		}
		s.rmu.Unlock()
		return n, nil
	}
	err := s.readErr
	s.rmu.Unlock()
	if err != nil {
		return 0, err
	}
	return 0, io.EOF
}

// Write sends payload bytes to the remote side.
func (s *Stream) Write(p []byte) (int, error) {
	s.wmu.Lock()
	closed := s.closeSent
	s.wmu.Unlock()
	if closed {
		return 0, errors.New("stream already closed for writing")
	}
	if err := s.m.send(TypeData, s.id, p); err != nil {
		s.markDead(err)
		return 0, err
	}
	return len(p), nil
}

// Close half-closes the write side: sends one CLOSE frame and refuses
// further writes. Safe to call more than once.
func (s *Stream) Close() {
	s.wmu.Lock()
	if s.closeSent {
		s.wmu.Unlock()
		return
	}
	s.closeSent = true
	s.wmu.Unlock()
	s.m.send(TypeClose, s.id, nil)
}

func (s *Stream) remoteClose() {
	s.rmu.Lock()
	s.readEOF = true
	s.rcond.Broadcast()
	s.rmu.Unlock()
}

func (s *Stream) markDead(err error) {
	s.m.markDead()
	s.rmu.Lock()
	s.readErr = err
	s.readEOF = true
	s.rcond.Broadcast()
	s.rmu.Unlock()
	s.m.remove(s.id)
}

// terminate is called when the mux connection dies.
func (s *Stream) terminate(err error) {
	s.m.mu.Lock()
	delete(s.m.streams, s.id)
	s.m.mu.Unlock()
	s.rmu.Lock()
	s.readErr = err
	s.readEOF = true
	s.rcond.Broadcast()
	s.rmu.Unlock()
}

// Release frees the stream from its mux map and is idempotent. Use it when a
// stream is being abandoned (e.g. a rejected route) rather than relayed.
func (s *Stream) Release() { s.finish() }

// finish removes a stream from its mux and releases any reader still blocked on
// it. It is called after a Relay/BridgeStreams completes so finished streams do
// not accumulate in the mux map. idempotent (concurrent with main loop teardown).
func (s *Stream) finish() {
	s.m.mu.Lock()
	delete(s.m.streams, s.id)
	s.m.mu.Unlock()
	s.rmu.Lock()
	s.readEOF = true
	s.rcond.Broadcast()
	s.rmu.Unlock()
}

// Relay pipes remote<->local until both directions finish, then closes local.
// Half-close semantics: when the stream's peer half-closes (request sent), the
// local write side is half-closed so the local peer sees EOF yet may keep
// responding; when the local peer half-closes, the stream write side is
// half-closed so the tunnel peer sees EOF. This preserves
// request/response with keep-alive.
func (s *Stream) Relay(local net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	// local -> stream
	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := local.Read(buf)
			if n > 0 {
				addUp(n)
				if _, werr := s.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		s.Close()
	}()
	// stream -> local
	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := s.Read(buf)
			if n > 0 {
				addDown(n)
				if _, werr := local.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		closeWrite(local)
	}()
	wg.Wait()
	local.Close()
	s.finish()
}

// closeWrite half-closes a TCP connection's write side (sends FIN) so the peer
// sees EOF. For non-TCP connections it falls back to a full Close.
func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.CloseWrite()
		return
	}
	c.Close()
}

// BridgeStreams pipes two logical streams together (used by the server to join
// an entry stream to a NAT stream). Both directions are forwarded until each
// side half-closes, then both are closed.
func BridgeStreams(a, b *Stream) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := a.Read(buf)
			if n > 0 {
				addUp(n)
				if _, werr := b.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		b.Close()
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := b.Read(buf)
			if n > 0 {
				addDown(n)
				if _, werr := a.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		a.Close()
	}()
	wg.Wait()
	a.Close()
	b.Close()
	a.finish()
	b.finish()
}
