// Command natclient exposes local services that live behind NAT through the
// server, so the server can route inbound connection streams to them. It dials
// the server outbound (traversing NAT), registers its identity + services, and
// stays connected. When the server sends an inbound stream, the NAT client
// dials the matching local service and relays bytes both ways.
package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"deepseekaiworker/internal/mux"
	"deepseekaiworker/internal/service"
	"deepseekaiworker/internal/tlscfg"
)

// ClientConfig is the on-disk JSON configuration for the NAT client.
type ClientConfig struct {
	Server string `json:"server"` // server address host:port

	// TLS trust (see internal/tlscfg): pick one.
	CAFile             string `json:"ca_file"`
	ServerFingerprint  string `json:"server_fingerprint"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify"`
	ServerName         string `json:"server_name"`

	ClientID              string            `json:"client_id"`
	Secret                string            `json:"secret"`
	Services              []mux.ServiceInfo `json:"services"`
	DialTimeoutSeconds    int               `json:"dial_timeout_seconds"`
	ReconnectDelaySeconds int               `json:"reconnect_delay_seconds"`
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to nat client config JSON")
	flag.Parse()

	// `deepseek-natclient service ...` manages the systemd unit (no config needed).
	if args := flag.Args(); len(args) > 0 && args[0] == "service" {
		os.Exit(service.RunCLI("natclient", *cfgPath, args[1:]))
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
	if cc.ClientID == "" {
		log.Fatal("config: 'client_id' is required")
	}
	if cc.Secret == "" {
		log.Fatal("config: 'secret' is required")
	}
	reconnect := time.Duration(cc.ReconnectDelaySeconds) * time.Second
	if reconnect <= 0 {
		reconnect = 5 * time.Second
	}
	if cc.DialTimeoutSeconds <= 0 {
		cc.DialTimeoutSeconds = 10
	}

	tlsCfg, err := tlsConf(&cc, cc.Server)
	if err != nil {
		log.Fatalf("tls: %v", err)
	}
	for _, svc := range cc.Services {
		log.Printf("service %-12s local %s", svc.Name, svcAddr(svc))
	}
	log.Printf("nat client starting; server=%s id=%s", cc.Server, cc.ClientID)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("shutting down")
		os.Exit(0)
	}()

	for {
		err := runSession(&cc, tlsCfg)
		if err != nil {
			log.Printf("session ended: %v", err)
		}
		log.Printf("reconnecting in %s...", reconnect)
		time.Sleep(reconnect)
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

func svcAddr(svc mux.ServiceInfo) string {
	if svc.Addr != "" {
		return svc.Addr
	}
	return net.JoinHostPort("127.0.0.1", fmt.Sprint(svc.Port))
}

// runSession maintains one connected session: registers services and serves
// inbound streams by dialing local services.
func runSession(cc *ClientConfig, tlsCfg *tls.Config) error {
	conn, err := tls.Dial("tcp", cc.Server, tlsCfg)
	if err != nil {
		return fmt.Errorf("dial server: %w", err)
	}
	log.Printf("connected to %s (tls)", cc.Server)

	m := mux.Dial(conn)
	reg := mux.Register{Role: mux.RoleNat, ClientID: cc.ClientID, Secret: cc.Secret, Services: cc.Services}
	b, err := mux.EncodeRegister(reg)
	if err != nil {
		conn.Close()
		return err
	}
	if err := m.SendRegister(b); err != nil {
		conn.Close()
		return fmt.Errorf("register: %w", err)
	}
	log.Printf("registered as NAT client %q", cc.ClientID)

	byPort := map[int]string{}
	for _, svc := range cc.Services {
		byPort[svc.Port] = svcAddr(svc)
	}
	timeout := time.Duration(cc.DialTimeoutSeconds) * time.Second

	m.SetOnOpen(func(s *mux.Stream, port uint16) {
		addr, ok := byPort[int(port)]
		if !ok {
			log.Printf("inbound stream for unknown service port %d", port)
			s.Close()
			return
		}
		log.Printf("serve local %s", addr)
		tc, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			log.Printf("dial local %s: %v", addr, err)
			s.Close()
			return
		}
		go s.Relay(tc)
	})

	<-m.DeadChan()
	return nil
}
