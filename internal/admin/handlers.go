package admin

// HTTP handlers for the console: captcha, login/logout, setup (init mode),
// dashboard, config editing/saving, and config export.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"deepseekaiworker/internal/config"
)

type loginData struct {
	Error      string
	Captcha    bool
	InitMode   bool
	HasAccount bool
}

func (s *Server) loginViewData(errMsg string, captcha bool) loginData {
	return loginData{Error: errMsg, Captcha: captcha, InitMode: s.isInitMode(), HasAccount: !s.isInitMode()}
}

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

func (s *Server) handleCaptcha(w http.ResponseWriter, r *http.Request) {
	token, png, err := s.captcha.New()
	if err != nil {
		http.Error(w, "captcha error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	// include token so the login form can submit it
	w.Header().Set("X-Captcha-Token", token)
	w.Write(png)
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
	captcha := r.Method == http.MethodPost
	if captcha {
		if s.rateLimit(w, r) {
			return
		}
		if err := r.ParseForm(); err != nil {
			s.render(w, "login.html", s.loginViewData("bad form", true))
			return
		}
		// captcha verify
		if !s.captcha.Verify(r.Form.Get("captcha_token"), r.Form.Get("captcha")) {
			s.recordFailure(s.ClientIP(r))
			s.render(w, "login.html", s.loginViewData("验证码错误", true))
			return
		}
		user := r.Form.Get("username")
		pass := r.Form.Get("password")
		cfg := s.GetConfig()
		if cfg == nil || cfg.Admin == nil || user != cfg.Admin.Username ||
			!VerifyPassword(cfg.Admin.PasswordHash, pass) {
			s.recordFailure(s.ClientIP(r))
			s.render(w, "login.html", s.loginViewData("账号或密码错误", true))
			return
		}
		s.resetAttempts(s.ClientIP(r))
		tok := s.sess.Create()
		secure := cfg.Admin.TLS
		setSessionCookie(w, tok, s.sess.ttl, secure)
		http.Redirect(w, r, "/dashboard", http.StatusFound)
		return
	}
	// GET
	token, png, err := s.captcha.New()
	_ = token
	if err != nil {
		http.Error(w, "captcha error", 500)
		return
	}
	_ = png
	s.render(w, "login.html", s.loginViewData("", true))
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

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !s.isInitMode() {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			s.render(w, "setup.html", map[string]any{"Error": "bad form"})
			return
		}
		if !s.captcha.Verify(r.Form.Get("captcha_token"), r.Form.Get("captcha")) {
			s.render(w, "setup.html", map[string]any{"Error": "验证码错误"})
			return
		}
		user := strings.TrimSpace(r.Form.Get("username"))
		pass := r.Form.Get("password")
		pass2 := r.Form.Get("password2")
		if len(user) < 3 {
			s.render(w, "setup.html", map[string]any{"Error": "用户名至少3个字符"})
			return
		}
		if len(pass) < 8 {
			s.render(w, "setup.html", map[string]any{"Error": "密码至少8位"})
			return
		}
		if pass != pass2 {
			s.render(w, "setup.html", map[string]any{"Error": "两次密码不一致"})
			return
		}
		hash, err := HashPassword(pass)
		if err != nil {
			s.render(w, "setup.html", map[string]any{"Error": "hashing error"})
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
			s.render(w, "setup.html", map[string]any{"Error": fmt.Sprintf("保存失败: %v", err)})
			return
		}
		s.logger.Printf("admin account created; leaving init mode")
		tok := s.sess.Create()
		setSessionCookie(w, tok, s.sess.ttl, next.Admin.TLS)
		http.Redirect(w, r, "/dashboard", http.StatusFound)
		return
	}
	// GET
	s.render(w, "setup.html", map[string]any{})
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	cfg := s.GetConfig()
	s.render(w, "dashboard.html", map[string]any{"Config": cfg})
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
		"_comment":                "Set server to your public server host:port; choose a TLS trust mode (ca_file or server_fingerprint).",
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
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Write(append(buf, '\n'))
}
