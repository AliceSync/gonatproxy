// Package admin implements the web management console embedded in the server.
// It provides captcha+username/password login, an initialization mode when no
// account is configured, full configuration editing, and configuration export
// for the entry client and NAT client.
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

// Server is the web console.
type Server struct {
	ConfigPath string
	// Reload is called after a config edit, with full control of re-applying.
	Reload func() error
	// GetConfig returns a deep copy of the current config for the console.
	GetConfig func() *config.ServerConfig
	// ApplyConfig persists + applies a config document from the console.
	ApplyConfig func(next *config.ServerConfig) error

	logger *log.Logger

	captcha *captcha
	sess    *sessions
	// login attempts rate limiting
	attempts   map[string]*rateEntry // keyed by client IP
	attemptsMu sync.Mutex

	tmpl *template.Template
}

type rateEntry struct {
	failures  int
	windowEnd time.Time
}

func New(cfgPath string, get func() *config.ServerConfig, apply func(*config.ServerConfig) error, reload func() error) *Server {
	s := &Server{
		ConfigPath:  cfgPath,
		Reload:      reload,
		GetConfig:   get,
		ApplyConfig: apply,
		logger:      log.New(log.Writer(), "[admin] ", log.LstdFlags),
		captcha:     newCaptcha(5 * time.Minute),
		sess:        newSessions(24 * time.Hour),
		attempts:    map[string]*rateEntry{},
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

// Run serves the console until ctx is cancelled. If keyFile is non-empty the
// console is served over TLS using certFile/keyFile.
func (s *Server) Run(ctx context.Context, listen, certFile, keyFile string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/setup", s.handleSetup)
	mux.HandleFunc("/captcha", s.handleCaptcha)
	mux.HandleFunc("/dashboard", s.requireAuth(s.handleDashboard))
	mux.HandleFunc("/config", s.requireAuth(s.handleConfigPage))
	mux.HandleFunc("/api/config", s.requireAuth(s.handleConfigJSON))
	mux.HandleFunc("/api/config/save", s.requireAuth(s.handleConfigSave))
	mux.HandleFunc("/api/export/client", s.requireAuth(s.handleExportClient))
	mux.HandleFunc("/api/export/nat", s.requireAuth(s.handleExportNAT))
	mux.HandleFunc("/static/", s.handleStatic)

	hs := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		hs.Close()
	}()
	s.logger.Printf("web console on %s (init_mode=%v)", listen, s.isInitMode())
	if keyFile != "" {
		s.logger.Printf("web console TLS enabled")
		return hs.ListenAndServeTLS(certFile, keyFile)
	}
	return hs.ListenAndServe()
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
	return hexEncode(b)
}
