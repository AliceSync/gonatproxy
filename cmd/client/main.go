// Command client is the "entry" tunnel endpoint that runs alongside an AI
// workflow on the machine. It dials the server over TLS, identifies itself,
// learns which local ports to bind from the server, listens on those ports,
// and tunnels every byte of each inbound local connection to the server,
// which forwards it to the corresponding target (server-side or a NAT client).
// Only the server address lives in the client config; everything else is
// decided by the server.
package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"deepseekaiworker/internal/mux"
	"deepseekaiworker/internal/proxy"
	"deepseekaiworker/internal/service"
	"deepseekaiworker/internal/tlscfg"
)

// ClientConfig is the on-disk JSON configuration for the entry client.
type ClientConfig struct {
	// Server is the server address host:port to dial over TLS.
	Server string `json:"server"`
	// TLS trust (see internal/tlscfg): pick one.
	CAFile             string `json:"ca_file"`
	ServerFingerprint  string `json:"server_fingerprint"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify"`
	ServerName         string `json:"server_name"`

	// Proxy is an optional forward proxy to reach the server, e.g.
	// "socks5://127.0.0.1:1080", "socks4/4a://...", "http://...".
	Proxy string `json:"proxy,omitempty"`

	ReconnectDelaySeconds int `json:"reconnect_delay_seconds"`
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to client config JSON")
	flag.Parse()

	// `deepseek-client service ...` manages the systemd unit (no config needed).
	if args := flag.Args(); len(args) > 0 && args[0] == "service" {
		os.Exit(service.RunCLI("client", *cfgPath, args[1:]))
	}

	cfgRaw, err := os.ReadFile(*cfgPath)
	if err != nil {
		log.Fatalf("read config: %v", err)
	}
	var cc ClientConfig
	if err := json.Unmarshal(cfgRaw, &cc); err != nil {
		log.Fatalf("parse config: %v", err)
	}
	if cc.Server == "" {
		log.Fatal("config: 'server' is required")
	}
	reconnect := time.Duration(cc.ReconnectDelaySeconds) * time.Second
	if reconnect <= 0 {
		reconnect = 5 * time.Second
	}

	tlsCfg, err := tlsConf(&cc, cc.Server)
	if err != nil {
		log.Fatalf("tls: %v", err)
	}

	log.Printf("entry client starting; server=%s", cc.Server)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("shutting down")
		os.Exit(0)
	}()

	for {
		// Exponential backoff (with ±20% jitter) on rapid reconnects: after an
		// outage the retry interval grows from the base up to a cap, and a
		// jittered delay plus the cap keeps a fleet of endpoints from
		// thundering-herding the server with a connection burst on recovery.
		base := reconnect
		delay := base
		if delay < time.Second {
			delay = time.Second
		}
		rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
		for {
			start := time.Now()
			err := runSession(cc, tlsCfg)
			if err != nil {
				log.Printf("session ended: %v", err)
			}
			lived := time.Since(start)
			if lived < 30*time.Second {
				delay *= 2
				if delay > 60*time.Second {
					delay = 60 * time.Second
				}
			} else {
				delay = base // healthy long-lived session → reset backoff
			}
			jitter := 1.0 + (rnd.Float64()-0.5)*0.4 // ±20%
			wait := time.Duration(float64(delay) * jitter)
			if wait < time.Second {
				wait = time.Second
			}
			log.Printf("reconnecting in %s (backoff %.0fs)...", wait, delay.Seconds())
			time.Sleep(wait)
		}
	}
}

func tlsConf(cc *ClientConfig, server string) (*tls.Config, error) {
	tc := tlscfg.Client{
		CAFile:             cc.CAFile,
		ServerFingerprint:  cc.ServerFingerprint,
		InsecureSkipVerify: cc.InsecureSkipVerify,
		ServerName:         cc.ServerName,
	}
	cfg, err := tc.Build()
	if err != nil {
		return nil, err
	}
	if cfg.ServerName == "" {
		if host, _, err := net.SplitHostPort(server); err == nil {
			cfg.ServerName = host
		}
	}
	return cfg, nil
}

// runSession maintains one TLS connection + set of local listeners until the
// connection dies, then returns.
func runSession(cc ClientConfig, tlsCfg *tls.Config) error {
	conn, err := proxy.DialTLS(cc.Proxy, cc.Server, tlsCfg, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial server (proxy=%q): %w", cc.Proxy, err)
	}
	log.Printf("connected to %s (tls)", cc.Server)

	m := mux.Dial(conn)
	if err := m.SendRegister(mustRegister(mux.Register{Role: mux.RoleEntry})); err != nil {
		conn.Close()
		return fmt.Errorf("register: %w", err)
	}

	cfgBytes, err := m.Config()
	if err != nil {
		conn.Close()
		return err
	}
	cfg, err := mux.DecodeConfig(cfgBytes)
	if err != nil {
		conn.Close()
		return fmt.Errorf("decode config: %w", err)
	}
	if len(cfg.Ports) == 0 {
		conn.Close()
		return fmt.Errorf("server sent no ports to listen on")
	}

	log.Printf("server instructed to listen on %d port(s):", len(cfg.Ports))
	for _, p := range cfg.Ports {
		log.Printf("  local port %5d (%-12s) -> %s", p.Port, p.Name, p.Target)
	}

	var listeners []net.Listener
	for _, p := range cfg.Ports {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p.Port))
		if err != nil {
			log.Printf("WARN: cannot listen on local port %d: %v", p.Port, err)
			continue
		}
		listeners = append(listeners, ln)
		go acceptLoop(ln, uint16(p.Port), m)
		log.Printf("listening on local port %d (%s)", p.Port, p.Name)
	}
	if len(listeners) == 0 {
		conn.Close()
		return fmt.Errorf("could not bind any requested local port")
	}

	<-m.DeadChan()
	for _, ln := range listeners {
		ln.Close()
	}
	return nil
}

// acceptLoop accepts local connections on ln and tunnels each over a new mux
// stream tagged with the port.
func acceptLoop(ln net.Listener, port uint16, m *mux.Mux) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		s, err := m.Open(port)
		if err != nil {
			log.Printf("open stream for port %d: %v", port, err)
			c.Close()
			continue // keep accepting; don't kill this listener on a transient error
		}
		go s.Relay(c)
	}
}

func mustRegister(r mux.Register) []byte {
	b, err := mux.EncodeRegister(r)
	if err != nil {
		log.Fatalf("encode register: %v", err)
	}
	return b
}
