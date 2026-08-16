package admin

import (
	"encoding/json"
	"mime/multipart"
	"net/http"
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

// postHelp runs a urlencoded POST against handler hh.
func postHelp(hh http.HandlerFunc, vals url.Values) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/x", strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hh(rec, req)
	return rec
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
	form := url.Values{}
	form.Set("username", "admin")
	form.Set("password", "supersecret99")
	form.Set("password2", "supersecret99")

	rec := postHelp(s.handleSetup, form)
	if rec.Code != 302 && rec.Code != 200 {
		t.Fatalf("setup code=%d body=%s", rec.Code, rec.Body.String())
	}
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

func TestSetupRejectsWeakPassword(t *testing.T) {
	s, _ := newTestServer()
	form := url.Values{}
	form.Set("username", "admin")
	form.Set("password", "short")
	form.Set("password2", "short")
	rec := postHelp(s.handleSetup, form)
	if !s.isInitMode() {
		t.Fatal("setup should fail: init mode remains on for weak password")
	}
	if rec.Code == 200 { // plain POST would re-render setup (200) not success
		// Acceptable: no account created
	}
	cur := s.GetConfig()
	if cur.Admin.Username != "" {
		t.Fatal("weak-password setup should not create account")
	}
}

func TestLoginWrongPasswordRejected(t *testing.T) {
	s, _ := newTestServer()
	// create account
	postHelp(s.handleSetup, url.Values{
		"username": {"admin"}, "password": {"hunter22222"}, "password2": {"hunter22222"},
	})
	rec := postHelp(s.handleLogin, url.Values{"username": {"admin"}, "password": {"wrong"}})
	// AJAX not set -> rejectAuth renders (200) or 401? For plain POST rejectAuth renders setup.html (200)
	if rec.Code != 200 && rec.Code != 401 {
		t.Fatalf("login wrong-pw code=%d", rec.Code)
	}
	// should not be authed
	if s.sess.Valid(s.sessionTokenV2(rec)) {
		t.Fatal("should not be authed with wrong password")
	}
}

func (s *Server) sessionTokenV2(r *httptest.ResponseRecorder) string {
	for _, c := range r.Result().Cookies() {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	return ""
}

func TestLoginAndConfigRoundtrip(t *testing.T) {
	s, getCur := newTestServer()
	// create account
	postHelp(s.handleSetup, url.Values{
		"username": {"admin"}, "password": {"hunter22222"}, "password2": {"hunter22222"},
	})
	// login (AJAX so we get a 200 + JSON on success)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{
		"username": {"admin"}, "password": {"hunter22222"},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "fetch")
	req.Header.Set("Accept", "application/json")
	s.handleLogin(rec, req)
	if rec.Code != 200 {
		t.Fatalf("login code=%d body=%s", rec.Code, rec.Body.String())
	}
	// set the returned session cookie on subsequent requests
	cookie := rec.Result().Cookies()[0]

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
	req2 := httptest.NewRequest("POST", "/api/config/save", strings.NewReader(save))
	req2.Header.Set("Content-Type", "application/json")
	req2.AddCookie(cookie)
	s.handleConfigSave(rec, req2)
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

// jsonUnmarshalPlain decodes JSON.
func jsonUnmarshalPlain(b []byte, v any) error { return json.Unmarshal(b, v) }

// TestSetupMultipart verifies the browser's native fetch(FormData) submit path,
// which is multipart/form-data — ParseForm alone would miss it.
func TestSetupMultipart(t *testing.T) {
	s, _ := newTestServer()
	var b strings.Builder
	mw := multipart.NewWriter(&b)
	_ = mw.WriteField("username", "admin")
	_ = mw.WriteField("password", "12345678")
	_ = mw.WriteField("password2", "12345678")
	mw.Close()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/setup", strings.NewReader(b.String()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Requested-With", "fetch")
	req.Header.Set("Accept", "application/json")
	s.handleSetup(rec, req)
	if rec.Code != 200 {
		t.Fatalf("setup multipart code=%d body=%s", rec.Code, rec.Body.String())
	}
	if s.isInitMode() {
		t.Fatal("account should be created via multipart")
	}
}

// TestServiceEndpoints verifies the services API routes respond and honor the
// host-provided service hooks (no systemctl/file side effects in tests).
func TestServiceEndpoints(t *testing.T) {
	s, _ := newTestServer()
	s.Hooks = &SystemHooks{}

	// no hooks configured => endpoints report unavailable
	rec := httptest.NewRecorder()
	s.handleServiceInspect(rec, httptest.NewRequest("GET", "/api/service", nil))
	var insp ServiceInspect
	if err := json.Unmarshal(rec.Body.Bytes(), &insp); err != nil {
		t.Fatalf("decode inspect: %v", err)
	}
	if insp.Unit != s.hooks().UnitIfAny() {
		t.Fatalf("unexpected unit %q", insp.Unit)
	}

	rec = httptest.NewRecorder()
	s.handleServiceGenerate(rec, httptest.NewRequest("POST", "/api/service/generate", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("generate w/o hooks code=%d", rec.Code)
	}

	// now provide hooks
	s.Hooks.Service = &ServiceHooks{
		Inspect: func() ServiceInspect {
			return ServiceInspect{Unit: "deepseek-server.service", Installed: true, Systemd: true, Managed: true, Config: "/etc/deepseek/server.json", RunUser: "deepseek", Exe: "/usr/local/bin/deepseek-server"}
		},
		Generate: func() (ServiceResult, error) {
			return ServiceResult{OK: "已写入：/etc/systemd/system/deepseek-server.service"}, nil
		},
		Control: func(action string) (ServiceResult, error) {
			if action == "enable" {
				return ServiceResult{OK: "已启用（自启）"}, nil
			}
			return ServiceResult{OK: "已执行 " + action}, nil
		},
	}
	rec = httptest.NewRecorder()
	s.handleServiceInspect(rec, httptest.NewRequest("GET", "/api/service", nil))
	if !strings.Contains(rec.Body.String(), "deepseek-server.service") {
		t.Fatalf("inspect missing unit: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.handleServiceGenerate(rec, httptest.NewRequest("POST", "/api/service/generate", nil))
	if !strings.Contains(rec.Body.String(), "已写入") {
		t.Fatalf("generate body: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.handleServiceControl(rec, httptest.NewRequest("POST", "/api/service/control?action=enable", nil))
	if !strings.Contains(rec.Body.String(), "已启用") {
		t.Fatalf("control enable body: %s", rec.Body.String())
	}

	// invalid action rejected
	rec = httptest.NewRecorder()
	s.handleServiceControl(rec, httptest.NewRequest("POST", "/api/service/control?action=delete", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid action code=%d", rec.Code)
	}
}
