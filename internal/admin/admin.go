// Package admin implements the web management console embedded in the server.
// It provides username/password login, an initialization mode when no
// account is configured, full configuration editing, a file manager, service
// management, and binary update + restart, plus configuration export for the
// entry client and NAT client.
package admin

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"deepseekaiworker/internal/config"
)

//go:embed templates static
var content embed.FS

// SystemHooks lets the host process provide privileged operations (restart,
// binary update) to the console. Nil fields hide the corresponding UI.
type SystemHooks struct {
	// Version returns the running binary version string.
	Version func() string
	// Grace is the graceful shutdown grace period.
	Grace time.Duration
	// Restart triggers a graceful restart of the server process.
	Restart func(grace time.Duration) error
	// StageDir is where uploaded update binaries are staged.
	StageDir string
	// ApplyUpdate moves a staged binary over the running binary and, if
	// restartNow, performs a graceful restart (fork start).
	ApplyUpdate func(stagedPath string, restartNow bool) (string, error)
	// SystemdUnit is the unit name when the server runs under systemd ("" otherwise).
	SystemdUnit string
	// Service provides systemd unit generation + lifecycle control. When nil the
	// services page hides the generate/control UI.
	Service *ServiceHooks
	// Metrics returns live connection/bandwidth counters for the dashboard
	// (nil hides the live metrics panel).
	Metrics func() ConnMetrics
}

// RatePoint is one 1-second sample of relay throughput + active connections.
type RatePoint struct {
	T     int64 `json:"t"`
	Up    int64 `json:"up"`    // bytes relayed up this second
	Down  int64 `json:"down"`  // bytes relayed down this second
	Conns int64 `json:"conns"` // total active relay connections
}

// ConnMetrics is the live dashboard snapshot.
type ConnMetrics struct {
	Entry   int64       `json:"entry"` // active entry (client) connections
	Nat     int64       `json:"nat"`   // active NAT client connections
	Total   int64       `json:"total"` // entry + nat
	Up      int64       `json:"up"`    // current up bytes/sec
	Down    int64       `json:"down"`  // current down bytes/sec
	History []RatePoint `json:"history"`
}

// ServiceInspect describes the current process/systemd state for the services
// page (mirrors what the host knows).
type ServiceInspect struct {
	Unit      string `json:"unit"`
	Installed bool   `json:"installed"`
	Systemd   bool   `json:"systemd"` // systemd present on host
	Managed   bool   `json:"managed"` // THIS process is a systemd service
	Daemon    bool   `json:"daemon"`  // launched via `start` (background daemon, not systemd)
	Exe       string `json:"exe"`
	Config    string `json:"config"`
	RunUser   string `json:"run_user"`
	Version   string `json:"version"`
}

// ServiceResult is a service-control outcome returned to the browser.
type ServiceResult struct {
	OK         string `json:"ok"`
	Restarting bool   `json:"restarting"`
}

// ServiceHooks lets the host provide systemd unit generation and lifecycle
// control (enable separate from start, graceful handoff into systemd).
type ServiceHooks struct {
	// Inspect returns the current state.
	Inspect func() ServiceInspect
	// Generate writes the unit file based on the current executable + config
	// path and runs daemon-reload. Returning the unit path.
	Generate func() (ServiceResult, error)
	// Control runs a lifecycle action: enable|disable|start|stop|restart|
	// daemon-reload. start performs a graceful handoff when the process was
	// launched via `start` (daemon) or foreground, so systemd takes over.
	Control func(action string) (ServiceResult, error)
}

// Server is the web console.
type Server struct {
	ConfigPath string
	// Reload is called after a config edit, with full control of re-applying.
	Reload func() error
	// GetConfig returns a deep copy of the current config for the console.
	GetConfig func() *config.ServerConfig
	// ApplyConfig persists + applies a config document from the console.
	ApplyConfig func(next *config.ServerConfig) error

	// FileRoot restricts the file manager to a directory. Empty = whole
	// filesystem. Set by the host to a sensible sandbox for the console.
	FileRoot string
	// DeployDir is where compiled binaries are found for the 部署助手 ("" disables).
	DeployDir string
	// Hooks provides privileged operations (restart/update).
	Hooks *SystemHooks

	logger *log.Logger

	sess *sessions
	// login attempts rate limiting
	attempts   map[string]*rateEntry // keyed by client IP
	attemptsMu sync.Mutex

	// short-lived deploy tokens used by the 部署助手 commands (target hosts have
	// no session, so a bearer token grants one-time-ish access to binaries/cfg).
	deployMu     sync.Mutex
	deployTok    map[string]time.Time // token -> expiry
	deployTokTTL time.Duration

	tmpl *template.Template
}

type rateEntry struct {
	failures  int
	windowEnd time.Time
}

func New(cfgPath string, get func() *config.ServerConfig, apply func(*config.ServerConfig) error, reload func() error) *Server {
	s := &Server{
		ConfigPath:   cfgPath,
		Reload:       reload,
		GetConfig:    get,
		ApplyConfig:  apply,
		logger:       log.New(log.Writer(), "[admin] ", log.LstdFlags),
		sess:         newSessions(24 * time.Hour),
		attempts:     map[string]*rateEntry{},
		deployTok:    map[string]time.Time{},
		deployTokTTL: 15 * time.Minute,
	}
	s.tmpl = mustParseTemplates()
	return s
}

func (s *Server) isInitMode() bool {
	cfg := s.GetConfig()
	if cfg == nil || cfg.Admin == nil {
		return true
	}
	return cfg.Admin.Username == "" || cfg.Admin.PasswordHash == ""
}

// handler returns the HTTP handler for the console (idempotent).
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/setup", s.handleSetup)
	mux.HandleFunc("/dashboard", s.requireAuth(s.handleDashboard))
	mux.HandleFunc("/config", s.requireAuth(s.handleConfigPage))
	mux.HandleFunc("/api/config", s.requireAuth(s.handleConfigJSON))
	mux.HandleFunc("/api/config/save", s.requireAuth(s.handleConfigSave))
	mux.HandleFunc("/api/export/client", s.requireAuth(s.handleExportClient))
	mux.HandleFunc("/api/export/nat", s.requireAuth(s.handleExportNAT))

	// file manager
	mux.HandleFunc("/files", s.requireAuth(s.handleFilesPage))
	mux.HandleFunc("/api/files/list", s.requireAuth(s.handleFilesList))
	mux.HandleFunc("/api/files/download", s.requireAuth(s.handleFileDownload))
	mux.HandleFunc("/api/files/read", s.requireAuth(s.handleFileRead))
	mux.HandleFunc("/api/files/write", s.requireAuth(s.handleFileWrite))
	mux.HandleFunc("/api/files/upload", s.requireAuth(s.handleFileUpload))
	mux.HandleFunc("/api/files/new", s.requireAuth(s.handleFileNew))
	mux.HandleFunc("/api/files/mkdir", s.requireAuth(s.handleFileMkdir))
	mux.HandleFunc("/api/files/delete", s.requireAuth(s.handleFileDelete))
	mux.HandleFunc("/api/files/rename", s.requireAuth(s.handleFileRename))

	// services
	mux.HandleFunc("/services", s.requireAuth(s.handleServicesPage))
	mux.HandleFunc("/api/systemctl", s.requireAuth(s.handleSystemctl))
	mux.HandleFunc("/api/service", s.requireAuth(s.handleServiceInspect))
	mux.HandleFunc("/api/service/generate", s.requireAuth(s.handleServiceGenerate))
	mux.HandleFunc("/api/service/control", s.requireAuth(s.handleServiceControl))

	// live metrics
	mux.HandleFunc("/api/metrics", s.requireAuth(s.handleMetrics))

	// system / update
	mux.HandleFunc("/system", s.requireAuth(s.handleSystemPage))
	mux.HandleFunc("/api/system/status", s.requireAuth(s.handleSystemStatus))
	mux.HandleFunc("/api/system/update", s.requireAuth(s.handleUpdate))
	mux.HandleFunc("/api/system/apply", s.requireAuth(s.handleApplyStaged))
	mux.HandleFunc("/api/system/restart", s.requireAuth(s.handleRestart))

	// deploy assistant
	mux.HandleFunc("/deploy", s.requireAuth(s.handleDeployPage))
	mux.HandleFunc("/api/deploy/status", s.requireAuth(s.handleDeployStatus))
	mux.HandleFunc("/api/deploy/token", s.requireAuth(s.handleDeployToken))
	mux.HandleFunc("/api/deploy/", s.handleDeployAsset) // session OR deploy token
	mux.HandleFunc("/static/", s.handleStatic)
	return mux
}

// Run serves the console until ctx is cancelled. If keyFile is non-empty the
// console is served over TLS using certFile/keyFile.
func (s *Server) Run(ctx context.Context, listen, certFile, keyFile string) error {
	if keyFile != "" {
		return s.RunTLS(ctx, listen, certFile, keyFile)
	}
	hs := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); hs.Close() }()
	s.logger.Printf("web console on %s (init_mode=%v)", listen, s.isInitMode())
	return hs.ListenAndServe()
}

// RunTLS serves the console over TLS on a self-managed listener.
func (s *Server) RunTLS(ctx context.Context, listen, certFile, keyFile string) error {
	hs := &http.Server{Addr: listen, Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); hs.Close() }()
	s.logger.Printf("web console (TLS) on %s (init_mode=%v)", listen, s.isInitMode())
	return hs.ListenAndServeTLS(certFile, keyFile)
}

// RunTLSOn serves the console over an externally provided TLS-capable listener.
// Used when the console shares the relay port.
func (s *Server) RunTLSOn(ctx context.Context, ln net.Listener) error {
	hs := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); hs.Close() }()
	s.logger.Printf("web console (TLS, shared port) on %s (init_mode=%v)", ln.Addr(), s.isInitMode())
	return hs.Serve(ln)
}

// RunPlainOn serves the console over an externally provided plain listener.
func (s *Server) RunPlainOn(ctx context.Context, ln net.Listener) error {
	hs := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); hs.Close() }()
	s.logger.Printf("web console on %s (init_mode=%v)", ln.Addr(), s.isInitMode())
	return hs.Serve(ln)
}

func (s *Server) ClientIP(r *http.Request) string {
	// support X-Forwarded-For? For a local console, use RemoteAddr (host part).
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if s.isInitMode() {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	if !s.isAuthed(r) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusFound)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Never cache authenticated pages: after logout/restart a stale cached copy
	// must never reappear looking like you're still logged in.
	w.Header().Set("Cache-Control", "no-store")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.logger.Printf("render %s: %v", name, err)
	}
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/static/")
	path := "static/" + rel
	b, err := content.ReadFile(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case strings.HasSuffix(path, ".css"):
		w.Header().Set("Content-Type", "text/css")
	case strings.HasSuffix(path, ".js"):
		w.Header().Set("Content-Type", "application/javascript")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Write(b)
}

func mustParseTemplates() *template.Template {
	t, err := template.New("root").ParseFS(content, "templates/*.html")
	if err != nil {
		panic(fmt.Sprintf("admin: embed templates: %v", err))
	}
	return t
}

// ---- JSON helpers ----
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.Encode(v)
}

func readBodyLimit(r *http.Request, limit int64) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if int64(len(buf)) > limit {
			return nil, fmt.Errorf("body too large")
		}
		if err != nil {
			return buf, nil // EOF
		}
	}
}

// randToken returns a random URL-usable token.
func randToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const hexDigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexDigits[v>>4]
		out[i*2+1] = hexDigits[v&0x0f]
	}
	return string(out)
}
