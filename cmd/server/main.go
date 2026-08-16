// Command server is the TLS relay hub + embedded web console.
//
// CLI:
//
//	deepseek-server [flags]            run in the foreground
//	deepseek-server start [flags]      daemonize (run in background)
//	deepseek-server version            print version and exit
//
// Flags: -config <path> defaults to config.json.
//
// If the config file does not exist, the server starts in initialization mode:
// only the web console runs, so the admin can create an account and generate a
// config from the UI. Once a config file exists, the relay + console both run.
//
// It accepts two kinds of outbound TLS client connections, distinguished by the
// REGISTER frame sent first: "entry" (binds local ports, forwards) and "nat"
// (behind NAT, exposes local services). The server holds the whole routing map:
// each entry port maps either to a server-side TCP target or a NAT client's
// registered service. Certificates are auto-reloaded for ACME.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"deepseekaiworker/internal/admin"
	"deepseekaiworker/internal/config"
	"deepseekaiworker/internal/mux"
)

var version = "dev"

// runtime holds the mutable routing state; all fields are guarded by mu so the
// web console can hot-reload config while connections reference it.
type runtime struct {
	mu          sync.RWMutex
	cfg         map[int]config.Route
	regSecrets  map[string]string
	entryConfig []byte
	dialTimeout time.Duration

	sc *config.ServerConfig // current full document (for console + reload)

	reg *registry
}

func newRuntime(sc *config.ServerConfig) *runtime {
	rt := &runtime{
		cfg:        map[int]config.Route{},
		regSecrets: map[string]string{},
		sc:         sc,
		reg:        newRegistry(),
	}
	rt.apply(sc) // build cfg/entryConfig/secrets/dialTimeout
	return rt
}

// apply rebuilds derived state from a config document. Caller holds no lock.
func (rt *runtime) apply(sc *config.ServerConfig) error {
	newCfg := map[int]config.Route{}
	newSecrets := map[string]string{}
	for id, nc := range sc.Clients {
		newSecrets[id] = nc.Secret
	}
	entryPorts := make([]mux.PortMapping, 0, len(sc.Routes))
	for _, r := range sc.Routes {
		if (r.Target == "") == (r.NatClient == "") {
			return fmt.Errorf("route %q: set exactly one of target or nat_client+service", r.Name)
		}
		newCfg[r.ListenPort] = r
		pm := mux.PortMapping{Name: r.Name, Port: r.ListenPort}
		if r.Target != "" {
			pm.Target = r.Target
		} else {
			pm.Target = "nat:" + r.NatClient + "/" + r.Service
		}
		entryPorts = append(entryPorts, pm)
	}
	enc, err := mux.EncodeConfig(mux.Config{Version: "2", Ports: entryPorts})
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	rt.mu.Lock()
	rt.cfg = newCfg
	rt.regSecrets = newSecrets
	rt.entryConfig = enc
	rt.dialTimeout = time.Duration(sc.DialTimeoutSeconds) * time.Second
	rt.sc = cloneConfig(sc)
	rt.mu.Unlock()
	return nil
}

func cloneConfig(sc *config.ServerConfig) *config.ServerConfig {
	if sc == nil {
		return nil
	}
	c := *sc
	c.Clients = map[string]*config.NatClient{}
	for k, v := range sc.Clients {
		if v == nil {
			c.Clients[k] = &config.NatClient{}
			continue
		}
		nc := *v
		nc.Services = append([]config.Service(nil), v.Services...)
		c.Clients[k] = &nc
	}
	c.Routes = append([]config.Route(nil), sc.Routes...)
	if sc.Admin != nil {
		a := *sc.Admin
		c.Admin = &a
	}
	return &c
}

// getConfig returns a deep-copied current config for the console.
func (rt *runtime) getConfig() *config.ServerConfig {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return cloneConfig(rt.sc)
}

// applyConfig persists + applies a new config document from the console.
func (rt *runtime) applyConfig(cfgPath string, next *config.ServerConfig) error {
	next.Normalize()
	if err := next.Validate(); err != nil {
		return err
	}
	// persist to disk
	if err := next.Save(cfgPath); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	// apply to runtime
	if err := rt.apply(next); err != nil {
		return err
	}
	log.Printf("config applied and reloaded")
	return nil
}

// natSession tracks one connected NAT client.
type natSession struct {
	addr   string
	m      *mux.Mux
	byName map[string]mux.ServiceInfo
	byPort map[int]string // local port -> dial addr
}

type registry struct {
	mu sync.Mutex
	m  map[string]*natSession
}

func newRegistry() *registry { return &registry{m: map[string]*natSession{}} }

func (r *registry) get(id string) *natSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[id]
}

func (r *registry) set(id string, s *natSession) {
	r.mu.Lock()
	r.m[id] = s
	r.mu.Unlock()
	log.Printf("NAT client online: %s from %s (%d service(s))", id, s.addr, len(s.byName))
}

func (r *registry) remove(id string, s *natSession) {
	r.mu.Lock()
	if r.m[id] == s {
		delete(r.m, id)
	}
	r.mu.Unlock()
	log.Printf("NAT client offline: %s", id)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// Extract config path from anywhere in argv because Go's flag package stops
	// at the first positional arg (e.g. `deepseek-server start -config x.json`).
	defCfg := "config.json"
	if v := flagValue(os.Args, "-config"); v != "" {
		defCfg = v
	}
	cfgPath := flag.String("config", defCfg, "path to server config JSON")
	versionFlag := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Println("deepseek-server", version)
		return
	}

	// subcommand handling: only "start" (daemonize) is special; everything else
	// runs in foreground. "run"/"foreground" are accepted as explicit no-ops.
	args := flag.Args()
	cmd := "run"
	if len(args) > 0 {
		switch args[0] {
		case "start":
			cmd = "start"
		case "run", "foreground":
			cmd = "run"
		case "version":
			fmt.Println("deepseek-server", version)
			return
		default:
			log.Fatalf("unknown command %q\nusage: deepseek-server [start|run] [-config path]", args[0])
		}
	}

	// load config; missing => init mode (web console only)
	cfg, loadErr := config.Load(*cfgPath)
	missing := loadErr != nil && (errors.Is(loadErr, os.ErrNotExist) || os.IsNotExist(loadErr))
	if loadErr != nil && !missing {
		log.Fatalf("load config: %v", loadErr)
	}

	if cmd == "start" {
		daemonize(*cfgPath, cfg, missing)
		return
	}
	// foreground below
	run(cfg, *cfgPath, missing)
}

func run(sc *config.ServerConfig, cfgPath string, initMode bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("shutting down")
		cancel()
		// give handlers a moment to return
		time.Sleep(300 * time.Millisecond)
		os.Exit(0)
	}()

	var rt *runtime
	if sc != nil {
		rt = newRuntime(sc)
	} else {
		// No config yet (init mode): build an empty runtime so the console can
		// edit/persist against it; the relay is not started below.
		rt = newRuntime(&config.ServerConfig{Admin: &config.Admin{}})
	}

	// Start the web console (always available; required alone in init mode).
	go func() {
		adm := admin.New(cfgPath,
			func() *config.ServerConfig { return rt.getConfig() },
			func(next *config.ServerConfig) error { return rt.applyConfig(cfgPath, next) },
			func() error { return nil },
		)
		var listen, cert, key string
		if rt.mu.RLock(); rt.sc != nil && rt.sc.Admin != nil {
			listen, cert, key = rt.sc.Admin.Listen, rt.sc.Admin.CertFile, rt.sc.Admin.KeyFile
			rt.mu.RUnlock()
		} else {
			rt.mu.RUnlock()
		}
		if listen == "" {
			listen = config.DefaultAdminListenAddr // nothing configured yet
		}
		if err := adm.Run(ctx, listen, cert, key); err != nil && ctx.Err() == nil {
			log.Printf("admin console: %v", err)
		}
	}()

	if initMode || sc == nil {
		log.Printf("initialization mode: web console only, no relay (create a config first)")
		<-ctx.Done()
		return
	}

	if err := startRelay(sc, rt, ctx); err != nil {
		log.Printf("relay: %v", err)
	}
}

func startRelay(sc *config.ServerConfig, rt *runtime, ctx context.Context) error {
	cm := &CertManager{certFile: sc.CertFile, keyFile: sc.KeyFile}
	go cm.Watch(time.Duration(sc.CertCheckSeconds) * time.Second)
	tlsCfg := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: cm.GetCertificate,
	}
	ln, err := tls.Listen("tcp", sc.Listen, tlsCfg)
	if err != nil {
		return fmt.Errorf("relay listen %s: %w", sc.Listen, err)
	}
	log.Printf("relay listening on %s (ACME cert reload every %ds)", sc.Listen, sc.CertCheckSeconds)
	logRouteSummary(rt)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("accept: %v", err)
			continue
		}
		go handleConnection(conn, rt)
	}
}

func logRouteSummary(rt *runtime) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	for _, r := range rt.cfg {
		dst := r.Target
		if dst == "" {
			dst = fmt.Sprintf("nat:%s/%s", r.NatClient, r.Service)
		}
		log.Printf("  entry port %5d (%-12s) -> %s", r.ListenPort, r.Name, dst)
	}
}

// flagValue scans args for "-key value" (also supports "-key=value").
func flagValue(args []string, key string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == key {
			return args[i+1]
		}
	}
	for _, a := range args {
		if len(a) > len(key)+1 && a[:len(key)] == key && a[len(key)] == '=' {
			return a[len(key)+1:]
		}
	}
	// empty string value filtering
	for i := 0; i < len(args)-1; i++ {
		if args[i] == key && args[i+1] == "" {
			return ""
		}
	}
	return ""
}
