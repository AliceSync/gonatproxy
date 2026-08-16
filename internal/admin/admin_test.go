package admin

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"deepseekaiworker/internal/config"
)

func newTestServer() (*Server, func() *config.ServerConfig) {
	cur := &config.ServerConfig{
		Listen:   ":9443",
		CertFile: "certs/server.crt",
		KeyFile:  "certs/server.key",
		Admin:    &config.Admin{Listen: "127.0.0.1:8444"},
		Clients:  map[string]*config.NatClient{},
	}
	s := New("/tmp/test-server.json",
		func() *config.ServerConfig { return cur },
		func(n *config.ServerConfig) error { cur = n; return nil },
		func() error { return nil },
	)
	return s, func() *config.ServerConfig { return cur }
}

// newCaptchaToken issues a captcha and returns the token + answer (white box).
func newCaptchaToken(s *Server) (string, string) {
	_, _, _ = s.captcha.New()
	s.captcha.mu.Lock()
	defer s.captcha.mu.Unlock()
	for t, e := range s.captcha.codes {
		return t, e.code
	}
	return "", ""
}

func TestPasswordHashRoundtrip(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(h, "correct horse") {
		t.Fatal("valid password should verify")
	}
	if VerifyPassword(h, "wrong") {
		t.Fatal("wrong password should not verify")
	}
	if h == "" || strings.HasPrefix(h, "pbkdf2-sha256$") == false {
		t.Fatalf("unexpected hash format: %q", h)
	}
}

func TestSetupCreatesAccount(t *testing.T) {
	s, _ := newTestServer()
	if !s.isInitMode() {
		t.Fatal("should start in init mode")
	}
	tok, code := newCaptchaToken(s)

	form := url.Values{}
	form.Set("captcha_token", tok)
	form.Set("captcha", code)
	form.Set("username", "admin")
	form.Set("password", "supersecret99")
	form.Set("password2", "supersecret99")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/setup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleSetup(rec, req)

	if rec.Code != 302 && rec.Code != 200 {
		t.Fatalf("setup code=%d body=%s", rec.Code, rec.Body.String())
	}
	// init mode off now
	if s.isInitMode() {
		t.Fatal("init mode should be off after setup")
	}
	cur := s.GetConfig()
	if cur.Admin == nil || cur.Admin.Username != "admin" || cur.Admin.PasswordHash == "" {
		t.Fatalf("admin not persisted: %+v", cur.Admin)
	}
	if !VerifyPassword(cur.Admin.PasswordHash, "supersecret99") {
		t.Fatal("stored password hash should verify")
	}
}

func TestSetupRejectsBadCaptcha(t *testing.T) {
	s, _ := newTestServer()
	tok, _ := newCaptchaToken(s)
	form := url.Values{}
	form.Set("captcha_token", tok)
	form.Set("captcha", "XXXXX")
	form.Set("username", "admin")
	form.Set("password", "supersecret99")
	form.Set("password2", "supersecret99")
	rec := httptest.NewRecorder()
	s.handleSetup(rec, httptest.NewRequest("POST", "/setup", strings.NewReader(form.Encode())))
	if !s.isInitMode() {
		t.Fatal("init mode should remain on when captcha is wrong")
	}
}

func TestLoginAndConfigRoundtrip(t *testing.T) {
	s, getCur := newTestServer()
	// establish an account via setup
	func() {
		tok, code := newCaptchaToken(s)
		form := url.Values{}
		form.Set("captcha_token", tok)
		form.Set("captcha", code)
		form.Set("username", "admin")
		form.Set("password", "hunter22222")
		form.Set("password2", "hunter22222")
		rec := httptest.NewRecorder()
		s.handleSetup(rec, httptest.NewRequest("POST", "/setup", strings.NewReader(form.Encode())))
	}()

	// login
	tok, code := newCaptchaToken(s)
	form := url.Values{}
	form.Set("captcha_token", tok)
	form.Set("captcha", code)
	form.Set("username", "admin")
	form.Set("password", "hunter22222")
	rec := httptest.NewRecorder()
	s.handleLogin(rec, httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode())))
	if rec.Code != 302 {
		t.Fatalf("login code=%d body=%s", rec.Code, rec.Body.String())
	}

	// save a full config with routes + client
	save := `{
	  "config": {
	    "listen": ":9443",
	    "cert_file":"certs/server.crt","key_file":"certs/server.key",
	    "cert_check_seconds":30,"dial_timeout_seconds":10,
	    "public_addr":"1.2.3.4:9443",
	    "clients": {"nat1":{"secret":"abc123","services":[{"name":"web","port":8080,"addr":"127.0.0.1:8080"}]}},
	    "routes":[{"name":"deepseek","listen_port":5051,"target":"127.0.0.1:8000"},
	               {"name":"h1","listen_port":5052,"nat_client":"nat1","service":"web"}],
	    "admin":{"listen":"127.0.0.1:8444"}
	  },
	  "new_password":""
	}`
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/config/save", strings.NewReader(save))
	req.Header.Set("Content-Type", "application/json")
	s.handleConfigSave(rec, req)
	if rec.Code != 200 {
		t.Fatalf("save code=%d body=%s", rec.Code, rec.Body.String())
	}

	cur := getCur()
	if len(cur.Routes) != 2 || cur.Clients["nat1"] == nil || cur.Clients["nat1"].Secret != "abc123" {
		t.Fatalf("config not saved: %+v", cur.Routes)
	}
}

func TestExportEndpoints(t *testing.T) {
	s, _ := newTestServer()
	// inject some data
	cur := s.GetConfig()
	cur.Routes = []config.Route{{Name: "d", ListenPort: 5051, Target: "127.0.0.1:8000"}}
	cur.Clients["nat1"] = &config.NatClient{Secret: "sec", Services: []config.Service{{Name: "web", Port: 8080, Addr: "127.0.0.1:8080"}}}
	cur.PublicAddr = "10.0.0.5:9443"

	c := httptest.NewRecorder()
	s.handleExportClient(c, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(c.Body.String(), "10.0.0.5:9443") {
		t.Fatalf("client export missing server addr: %s", c.Body.String())
	}
	n := httptest.NewRecorder()
	s.handleExportNAT(n, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(n.Body.String(), "nat1") || !strings.Contains(n.Body.String(), "sec") {
		t.Fatalf("nat export: %s", n.Body.String())
	}
}
