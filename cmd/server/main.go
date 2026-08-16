// Command server is the TLS relay hub + embedded web console.
//
// CLI:
//
//	deepseek-server [flags]            run in the foreground
//	deepseek-server start [flags]      daemonize (run in background)
//	deepseek-server service <sub>      manage the systemd unit (install/enable/start/…)
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
	"net"
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

// -- small accessors used at startup (read under no lock; called once) --
func (rt *runtime) scListen() string   { return rt.sc.Listen }
func (rt *runtime) scCertFile() string { return rt.sc.CertFile }
func (rt *runtime) scKeyFile() string  { return rt.sc.KeyFile }
func (rt *runtime) certCheckSecs() int { return rt.sc.CertCheckSeconds }
func (rt *runtime) scAdminListen() string {
	if rt.sc.Admin == nil {
		return ""
	}
	return rt.sc.Admin.Listen
}
func (rt *runtime) scAdminCert() (string, string) {
	if rt.sc.Admin == nil {
		return "", ""
	}
	return rt.sc.Admin.CertFile, rt.sc.Admin.KeyFile
}

// sameAddr reports whether a and b resolve to the same host:port pair, where
// an empty host means "all interfaces" (":8443" == "0.0.0.0:8443").
func sameAddr(a, b string) bool {
	ha, fa, _ := net.SplitHostPort(a)
	hb, fb, _ := net.SplitHostPort(b)
	if fa != fb {
		return false
	}
	ha, hb = normalizeHost(ha), normalizeHost(hb)
	return ha == hb
}

func normalizeHost(h string) string {
	if h == "" || h == "0.0.0.0" || h == "::" || h == "[::]" {
		return ""
	}
	return h
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

// applyLoose persists without full validation. Used only when creating the
// first admin account (no routes yet); routes are added afterwards via the UI.
func (rt *runtime) applyLoose(cfgPath string, next *config.ServerConfig) error {
	next.Normalize()
	if err := next.Save(cfgPath); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := rt.apply(next); err != nil {
		return err
	}
	log.Printf("admin account saved (init mode)")
	return nil
}

// hasAdmin reports whether an admin account is configured.
func (rt *runtime) hasAdmin() bool {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.sc != nil && rt.sc.Admin != nil && rt.sc.Admin.Username != ""
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
		case "service":
			// e.g. `deepseek-server service install|enable|start|...`
			os.Exit(runServiceCLI(*cfgPath, args[1:]))
		default:
			log.Fatalf("unknown command %q\nusage: deepseek-server [start|run|service install] [-config path]", args[0])
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
		time.Sleep(300 * time.Millisecond)
		os.Exit(0)
	}()

	// A runtime always exists (empty in init mode) so the console can edit it.
	var rt *runtime
	if sc != nil {
		rt = newRuntime(sc)
	} else {
		rt = newRuntime(&config.ServerConfig{Admin: &config.Admin{}})
	}
	relayOK := !(initMode || sc == nil || len(sc.Routes) == 0)

	adm := admin.New(cfgPath,
		func() *config.ServerConfig { return rt.getConfig() },
		func(next *config.ServerConfig) error {
			if !rt.hasAdmin() {
				// first account creation: allow a config with no routes yet
				return rt.applyLoose(cfgPath, next)
			}
			return rt.applyConfig(cfgPath, next)
		},
		func() error { return nil },
	)
	adm.Hooks = buildAdminHooks(cfgPath)

	// Certificates: ACME files if present, else ephemeral in-memory self-signed.
	cm := newCertManager(rt.scCertFile(), rt.scKeyFile())
	cm.Ensure()
	go cm.Watch(time.Duration(rt.certCheckSecs()) * time.Second)
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: cm.GetCertificate}

	// Port layout: shared (console+relay on one TLS port) by default, or
	// independent if admin.listen is explicitly set and differs from relay.
	adminListen := rt.scAdminListen()
	shared := adminListen == "" || sameAddr(adminListen, rt.scListen())

	if shared {
		listenAddr := rt.scListen()
		if listenAddr == "" {
			listenAddr = config.DefaultListenAddr
		}
		if initMode || sc == nil {
			// init mode: only the console runs; allow port fallback if busy.
			running, err := startSharedInit(ctx, listenAddr, tlsCfg, adm)
			if err != nil {
				log.Printf("announce: %v", err)
			}
			log.Printf("initialization mode: web console only (https) on %s — no relay until a config is saved", running)
			<-ctx.Done()
			return
		}
		// full mode with shared port
		addr, err := startShared(ctx, listenAddr, tlsCfg, rt, relayOK, adm)
		if err != nil {
			log.Printf("shared listen %s: %v", listenAddr, err)
		} else {
			logRouteSummary(rt)
			log.Printf("console + relay share TLS port %s", addr)
		}
		<-ctx.Done()
		return
	}

	// independent: console on its own listener
	cert, key := rt.scAdminCert()
	if key != "" {
		cert, key = rt.scCertFile(), rt.scKeyFile()
	}
	if initMode || sc == nil {
		log.Printf("initialization mode: web console only on %s", adminListen)
		<-ctx.Done()
		return
	}
	go func() {
		if err := runIndependent(ctx, adminListen, cert, key, adm); err != nil && ctx.Err() == nil {
			log.Printf("admin console: %v", err)
		}
	}()
	if err := startRelay(sc, rt, ctx, tlsCfg); err != nil {
		log.Printf("relay: %v", err)
	}
}

// startSharedInit runs the web console on a single TLS port in init mode, with
// automatic fallback to an ephemeral port if the default is busy.
func startSharedInit(ctx context.Context, addr string, tlsCfg *tls.Config, adm *admin.Server) (string, error) {
	raw, err := net.Listen("tcp", addr)
	if err != nil && isAddrInUse(err) {
		fallback, ferr := net.Listen("tcp", "127.0.0.1:0")
		if ferr != nil {
			return "", ferr
		}
		log.Printf("WARNING: %s busy; using ephemeral port %d for web console", addr, fallback.Addr().(*net.TCPAddr).Port)
		raw = fallback
	} else if err != nil {
		return "", err
	}
	go func() { <-ctx.Done(); raw.Close() }()
	cl := &channelListener{ch: make(chan net.Conn, 64), addr: raw.Addr(), closed: make(chan struct{})}
	sl := &sharedListener{raw: raw, tlsCfg: tlsCfg, console: cl, rt: nil, relayOK: false, ctx: ctx}
	sl.raw = raw
	go sl.loop()
	go func() {
		if err := adm.RunTLSOn(ctx, cl); err != nil && ctx.Err() == nil {
			log.Printf("admin console: %v", err)
		}
	}()
	return raw.Addr().String(), nil
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

// startRelay runs the relay server on its own TLS listener (independent-port
// mode; the console is served separately). Blocks until ctx is cancelled.
func startRelay(sc *config.ServerConfig, rt *runtime, ctx context.Context, tlsCfg *tls.Config) error {
	ln, err := tls.Listen("tcp", sc.Listen, tlsCfg)
	if err != nil {
		return fmt.Errorf("relay listen %s: %w", sc.Listen, err)
	}
	registerStop(func() { ln.Close() })
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
