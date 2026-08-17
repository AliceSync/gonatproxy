package admin

// HTTP handlers for the console: login/logout, setup (init mode),
// dashboard, config editing/saving, and config export.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"deepseekaiworker/internal/config"
)

type loginData struct{}

// rateLimit returns whether the IP is blocked from further login attempts.
func (s *Server) rateLimit(w http.ResponseWriter, r *http.Request) bool {
	ip := s.ClientIP(r)
	now := time.Now()
	s.attemptsMu.Lock()
	e, ok := s.attempts[ip]
	if !ok || now.After(e.windowEnd) {
		e = &rateEntry{windowEnd: now.Add(10 * time.Minute)}
		s.attempts[ip] = e
	}
	if e.failures >= 5 {
		s.attemptsMu.Unlock()
		http.Error(w, "too many attempts, try later", http.StatusTooManyRequests)
		return true
	}
	s.attemptsMu.Unlock()
	return false
}

func (s *Server) recordFailure(ip string) {
	s.attemptsMu.Lock()
	defer s.attemptsMu.Unlock()
	e := s.attempts[ip]
	if e == nil || time.Now().After(e.windowEnd) {
		e = &rateEntry{windowEnd: time.Now().Add(10 * time.Minute)}
		s.attempts[ip] = e
	}
	e.failures++
}

func (s *Server) resetAttempts(ip string) {
	s.attemptsMu.Lock()
	delete(s.attempts, ip)
	s.attemptsMu.Unlock()
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.isInitMode() {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	if s.isAuthed(r) {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
		return
	}
	if r.Method == http.MethodPost {
		if s.rateLimit(w, r) {
			s.rejectAuth(w, r, "尝试次数过多，请稍后再试")
			return
		}
		if err := parseBody(r); err != nil {
			s.rejectAuth(w, r, "表单解析失败")
			return
		}
		user := r.Form.Get("username")
		pass := r.Form.Get("password")
		cfg := s.GetConfig()
		if cfg == nil || cfg.Admin == nil || user != cfg.Admin.Username ||
			!VerifyPassword(cfg.Admin.PasswordHash, pass) {
			s.recordFailure(s.ClientIP(r))
			s.rejectAuth(w, r, "账号或密码错误")
			return
		}
		s.resetAttempts(s.ClientIP(r))
		tok := s.sess.Create()
		setSessionCookie(w, tok, s.sess.ttl, s.isTLS())
		s.acceptAuth(w, r, "/dashboard")
		return
	}
	// GET
	s.render(w, "login.html", loginData{})
}

// isAJAX reports whether a request expects a JSON response (fetch-based forms).
func isAJAX(r *http.Request) bool {
	return r.Header.Get("X-Requested-With") == "fetch" ||
		strings.Contains(r.Header.Get("Accept"), "application/json")
}

// parseBody parses the request body into r.Form for both urlencoded and
// multipart/form-data. Request.ParseForm() alone handles urlencoded; multipart
// requires an explicit ParseMultipartForm call (which, conversely, errors on
// non-multipart bodies, so only invoke it when content-type matches).
func parseBody(r *http.Request) error {
	if err := r.ParseForm(); err != nil {
		return err
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return r.ParseMultipartForm(32 << 20)
	}
	return nil
}

// rejectAuth responds to a failed login/setup. For AJAX it returns a JSON
// error (so the page keeps the user's input); for plain GET it re-renders.
func (s *Server) rejectAuth(w http.ResponseWriter, r *http.Request, msg string) {
	if isAJAX(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": msg})
		return
	}
	// non-AJAX: render an error page
	s.render(w, "setup.html", map[string]any{"Error": msg, "Username": r.Form.Get("username")})
}

func (s *Server) acceptAuth(w http.ResponseWriter, r *http.Request, to string) {
	if isAJAX(r) {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true", "to": to})
		return
	}
	http.Redirect(w, r, to, http.StatusFound)
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !s.isInitMode() {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	if r.Method == http.MethodPost {
		if s.rateLimit(w, r) {
			s.rejectAuth(w, r, "尝试次数过多，请稍后再试")
			return
		}
		if err := parseBody(r); err != nil {
			s.rejectAuth(w, r, "表单解析失败")
			return
		}
		user := strings.TrimSpace(r.Form.Get("username"))
		pass := r.Form.Get("password")
		pass2 := r.Form.Get("password2")
		if len(user) < 3 {
			s.rejectAuth(w, r, "用户名至少3个字符")
			return
		}
		if len(pass) < 8 {
			s.rejectAuth(w, r, "密码至少8位")
			return
		}
		if pass != pass2 {
			s.rejectAuth(w, r, "两次密码不一致")
			return
		}
		hash, err := HashPassword(pass)
		if err != nil {
			s.rejectAuth(w, r, "密码加密失败")
			return
		}
		next := s.GetConfig()
		if next == nil {
			next = &config.ServerConfig{Admin: &config.Admin{}}
		}
		if next.Admin == nil {
			next.Admin = &config.Admin{}
		}
		next.Admin.Username = user
		next.Admin.PasswordHash = hash
		if err := s.ApplyConfig(next); err != nil {
			s.logger.Printf("apply config during setup: %v", err)
			s.rejectAuth(w, r, "保存失败: "+err.Error())
			return
		}
		s.logger.Printf("admin account created; leaving init mode")
		tok := s.sess.Create()
		setSessionCookie(w, tok, s.sess.ttl, next.Admin.TLS)
		s.acceptAuth(w, r, "/dashboard")
		return
	}
	// GET
	s.render(w, "setup.html", map[string]any{})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	sec := s.isTLS()
	s.sess.Destroy(s.sessionToken(r))
	clearSessionCookie(w, sec)
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *Server) isTLS() bool {
	if c := s.GetConfig(); c != nil && c.Admin != nil {
		return c.Admin.TLS
	}
	return false
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	cfg := s.GetConfig()
	s.render(w, "dashboard.html", map[string]any{"Config": cfg, "Svc": s.inspectService()})
}

func (s *Server) handleConfigPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "config.html", map[string]any{"Config": s.GetConfig()})
}

// handleConfigSave accepts a full config document and persists + applies it.
func (s *Server) handleConfigJSON(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.GetConfig())
}

// saveRequest is the config editor payload: the full config plus an optional
// new admin password (transient, never persisted in plaintext).
type saveRequest struct {
	Config      *config.ServerConfig `json:"config"`
	NewPassword string               `json:"new_password"`
}

// handleConfigSave accepts a full config document and persists + applies it.
func (s *Server) handleConfigSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := readBodyLimit(r, 1<<20)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var req saveRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.Config == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing config"})
		return
	}
	next := req.Config
	cur := s.GetConfig()
	if next.Admin == nil {
		next.Admin = &config.Admin{}
	}
	if cur != nil && cur.Admin != nil {
		if req.NewPassword != "" {
			np := strings.TrimSpace(req.NewPassword)
			if len(np) < 8 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "新密码至少8位"})
				return
			}
			h, herr := HashPassword(np)
			if herr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "password hashing failed"})
				return
			}
			next.Admin.PasswordHash = h
			next.Admin.Username = cur.Admin.Username
		} else if next.Admin.PasswordHash == "" {
			// keep existing credentials when the UI did not change them
			next.Admin.PasswordHash = cur.Admin.PasswordHash
			if next.Admin.Username == "" {
				next.Admin.Username = cur.Admin.Username
			}
		}
	}
	if err := s.ApplyConfig(next); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "saved"})
}

// handleExportClient returns a ready-to-use client.json.
func (s *Server) handleExportClient(w http.ResponseWriter, r *http.Request) {
	cfg := s.GetConfig()
	server := cfg.PublicAddr
	if server == "" {
		server = cfg.Listen
	}
	doc := map[string]any{
		"server":                  server,
		"ca_file":                 "",
		"server_fingerprint":      "",
		"insecure_skip_verify":    false,
		"server_name":             "",
		"reconnect_delay_seconds": 5,
		"_comment":                "TLS trust: leave empty=normal verification(public cert); or set server_fingerprint / ca_file / insecure_skip_verify.",
	}
	downloadJSON(w, "client.json", doc)
}

// handleExportNAT returns per-client natclient.json files for every client.
func (s *Server) handleExportNAT(w http.ResponseWriter, r *http.Request) {
	cfg := s.GetConfig()
	server := cfg.PublicAddr
	if server == "" {
		server = cfg.Listen
	}
	clients := map[string]config.NatClientView{}
	for id, nc := range cfg.Clients {
		clients[id] = config.NatClientView{
			Server:                server,
			ClientID:              id,
			Secret:                nc.Secret,
			ReconnectDelaySeconds: 5,
			DialTimeoutSeconds:    10,
			Services:              nc.Services,
		}
	}
	downloadJSON(w, "natclients.json", clients)
}

func downloadJSON(w http.ResponseWriter, filename string, v any) {
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		http.Error(w, "export error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Write(append(buf, '\n'))
}
