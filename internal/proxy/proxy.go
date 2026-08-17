// Package proxy dials outbound TCP connections through a forward HTTP/SOCKS
// proxy, so client/natclient can reach the server from restricted networks.
// Supported schemes: socks5, socks4, socks4a, http (CONNECT). Stdlib only.
package proxy

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// URL is a parsed proxy endpoint: scheme://[user:pass@]host:port.
type URL struct {
	Scheme string
	Host   string // host:port of the proxy
	User   string
	Pass   string
}

// Parse parses a proxy URL and validates the scheme + port.
func Parse(raw string) (*URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("proxy: %v", err)
	}
	p := &URL{Scheme: strings.ToLower(u.Scheme), Host: u.Host}
	if u.Port() == "" {
		return nil, fmt.Errorf("proxy: %s missing port", raw)
	}
	switch p.Scheme {
	case "socks5", "socks4", "socks4a", "http":
	default:
		return nil, fmt.Errorf("proxy: unsupported scheme %q (want socks5/socks4/socks4a/http)", u.Scheme)
	}
	if u.User != nil {
		p.User = u.User.Username()
		p.Pass, _ = u.User.Password()
	}
	return p, nil
}

func splitTarget(target string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(target)
	if err != nil {
		return "", "", fmt.Errorf("proxy: bad target %q", target)
	}
	return host, port, nil
}

// Dial connects to target through the proxy and returns the raw connection
// (caller wraps it in TLS). A timeout bounds each handshake.
func Dial(p *URL, target string, timeout time.Duration) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	c, err := d.Dial("tcp", p.Host)
	if err != nil {
		return nil, fmt.Errorf("proxy: dial %s: %v", p.Host, err)
	}
	if timeout > 0 {
		_ = c.SetDeadline(time.Now().Add(timeout))
	}
	defer func() { _ = c.SetDeadline(time.Time{}) }()

	switch p.Scheme {
	case "socks5":
		err = socks5Handshake(c, p, target)
	case "socks4", "socks4a":
		err = socks4Handshake(c, p, target, p.Scheme == "socks4a")
	case "http":
		c, err = httpConnect(c, p, target)
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// ---- SOCKS5 ----

func socks5Handshake(c net.Conn, p *URL, target string) error {
	host, port, err := splitTarget(target)
	if err != nil {
		return err
	}
	methods := []byte{0x00}
	if p.User != "" {
		methods = append(methods, 0x02)
	}
	greet := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := c.Write(greet); err != nil {
		return fmt.Errorf("proxy socks5: send greet: %v", err)
	}
	resp := make([]byte, 2)
	if _, err := ioReadFull(c, resp); err != nil {
		return fmt.Errorf("proxy socks5: read greet: %v", err)
	}
	if resp[0] != 0x05 {
		return fmt.Errorf("proxy socks5: bad version %d", resp[0])
	}
	if resp[1] == 0x02 { // user/pass auth
		u, pw := []byte(p.User), []byte(p.Pass)
		auth := []byte{0x01, byte(len(u))}
		auth = append(auth, u...)
		auth = append(auth, byte(len(pw)))
		auth = append(auth, pw...)
		if _, err := c.Write(auth); err != nil {
			return fmt.Errorf("proxy socks5: auth: %v", err)
		}
		if _, err := ioReadFull(c, resp); err != nil || resp[1] != 0x00 {
			return fmt.Errorf("proxy socks5: auth failed")
		}
	} else if resp[1] != 0x00 {
		return fmt.Errorf("proxy socks5: no acceptable auth method")
	}
	atyp, addrBytes := addrFor(host)
	portBE := make([]byte, 2)
	binary.BigEndian.PutUint16(portBE, atou16(port))
	req := []byte{0x05, 0x01, 0x00, atyp}
	req = append(req, addrBytes...)
	req = append(req, portBE...)
	if _, err := c.Write(req); err != nil {
		return fmt.Errorf("proxy socks5: connect: %v", err)
	}
	head := make([]byte, 4)
	if _, err := ioReadFull(c, head); err != nil {
		return fmt.Errorf("proxy socks5: reply: %v", err)
	}
	if head[1] != 0x00 {
		return fmt.Errorf("proxy socks5: connect failed, code %d", head[1])
	}
	return discardBoundAddr(c, head)
}

// discardBoundAddr reads and discards the variable-length bound address.
func discardBoundAddr(c net.Conn, head []byte) error {
	atyp := head[3]
	var n int
	switch atyp {
	case 0x01:
		n = 4
	case 0x03:
		if len(head) < 5 {
			return errors.New("proxy socks5: short reply")
		}
		n = 1 + int(head[4])
	case 0x04:
		n = 16
	default:
		return fmt.Errorf("proxy socks5: bad atyp %d", atyp)
	}
	buf := make([]byte, n+2)
	return readFullNull(c, buf)
}

// ---- SOCKS4 / SOCKS4a ----

func socks4Handshake(c net.Conn, p *URL, target string, allowHost bool) error {
	host, port, err := splitTarget(target)
	if err != nil {
		return err
	}
	var ipBytes []byte
	var hostname string
	if ip := net.ParseIP(host); ip != nil {
		ip4 := ip.To4()
		if ip4 != nil {
			ipBytes = ip4
		} else if ip.To16() != nil {
			return errors.New("proxy socks4: ipv6 target unsupported")
		}
	} else if allowHost {
		// SOCKS4a: send 0.0.0.x and append the hostname after the userid.
		ipBytes = []byte{0, 0, 0, 1}
		hostname = host
	} else {
		return fmt.Errorf("proxy socks4: hostname %q needs socks4a", host)
	}
	req := []byte{0x04, 0x01}
	pn := atou16(port)
	req = append(req, byte(pn>>8), byte(pn&0xff))
	req = append(req, ipBytes...)
	req = append(req, p.User...)
	req = append(req, 0x00)
	if hostname != "" {
		req = append(req, hostname...)
		req = append(req, 0x00)
	}
	if _, err := c.Write(req); err != nil {
		return fmt.Errorf("proxy socks4: send: %v", err)
	}
	resp := make([]byte, 8)
	if _, err := ioReadFull(c, resp); err != nil {
		return fmt.Errorf("proxy socks4: reply: %v", err)
	}
	if resp[1] != 0x5a {
		return fmt.Errorf("proxy socks4: connect failed, code %d", resp[1])
	}
	return nil
}

// ---- HTTP CONNECT ----

func httpConnect(c net.Conn, p *URL, target string) (net.Conn, error) {
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if p.User != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(p.User + ":" + p.Pass))
		req += "Proxy-Authorization: Basic " + cred + "\r\n"
	}
	req += "\r\n"
	if _, err := c.Write([]byte(req)); err != nil {
		return nil, fmt.Errorf("proxy http: send: %v", err)
	}
	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("proxy http: read status: %v", err)
	}
	// read headers until blank line
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("proxy http: read headers: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	if !strings.HasPrefix(status, "HTTP/1.1 200") && !strings.HasPrefix(status, "HTTP/1.0 200") {
		return nil, fmt.Errorf("proxy http: CONNECT failed: %s", strings.TrimSpace(status))
	}
	// any tunnel bytes already buffered must be preserved
	w := &bufferedConn{Conn: c, r: br}
	return w, nil
}

// bufferedConn keeps bytes the CONNECT response reader already consumed.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// ---- helpers ----

func ioReadFull(c net.Conn, b []byte) (int, error) {
	total := 0
	for total < len(b) {
		n, err := c.Read(b[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func readFullNull(c net.Conn, b []byte) error {
	_, err := ioReadFull(c, b)
	return err
}

// addrFor builds the SOCKS5 ATYP + address bytes (domain for hostnames).
func addrFor(host string) (byte, []byte) {
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			return 0x01, []byte(ip4)
		}
		return 0x04, []byte(ip.To16())
	}
	h := []byte(host)
	if len(h) > 255 {
		h = h[:255]
	}
	out := append([]byte{byte(len(h))}, h...)
	return 0x03, out
}

func atou16(s string) uint16 {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 65535 {
		return 0
	}
	return uint16(n)
}

// DialTLS connects to target (directly, or through proxyStr when non-empty),
// wraps the connection in TLS and completes the handshake. Returns a ready
// *tls.Conn. timeout bounds the dial + handshake (0 = no timeout).
func DialTLS(proxyStr, target string, tlsCfg *tls.Config, timeout time.Duration) (*tls.Conn, error) {
	var raw net.Conn
	var err error
	if proxyStr != "" {
		p, perr := Parse(proxyStr)
		if perr != nil {
			return nil, perr
		}
		raw, err = Dial(p, target, timeout)
	} else {
		d := &net.Dialer{Timeout: timeout}
		raw, err = d.Dial("tcp", target)
	}
	if err != nil {
		return nil, err
	}
	tc := tls.Client(raw, tlsCfg)
	if timeout > 0 {
		_ = tc.SetDeadline(time.Now().Add(timeout))
	}
	if err := tc.Handshake(); err != nil {
		tc.Close()
		return nil, err
	}
	_ = tc.SetDeadline(time.Time{})
	return tc, nil
}
