package admin

// 部署助手 (Deploy Assistant): helps hand binaries + ready-made client/natclient
// configs to the host machines that run those (they have no GUI of their own).
//
// Because the target hosts fetch from the server over plain curl (no browser
// session), the assistant issues short-lived bearer tokens so a copy-pasted
// command can download the binary and config without a login. Every asset
// endpoint also works with an authenticated session (so you can click through
// in the browser too).

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const deployTokenTTL = 15 * time.Minute

// deployBaseURL returns the https base for the console as reachable by the
// target, preferring the request host used to reach us.
func (s *Server) deployBaseURL(r *http.Request) string {
	host := r.Host
	cfg := s.GetConfig()
	if cfg != nil {
		if p := cfg.PublicAddr; p != "" {
			// PublicAddr may be host:port; keep a host
			if !strings.HasPrefix(p, ":") && !strings.Contains(p, "//") {
				host = p
			}
		}
	}
	if i := strings.IndexAny(host, "/?"); i >= 0 {
		host = host[:i]
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return "https://" + host
}

func (s *Server) handleDeployPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "deploy.html", map[string]any{"Enabled": s.DeployDir != ""})
}

var deployArchs = []string{"amd64", "arm64"}

// archBinaryPath returns the common `<slot>_linux_<arch>` binary path.
func (s *Server) archBinaryPath(slot, arch string) string {
	if s.DeployDir == "" {
		return ""
	}
	return filepath.Join(s.DeployDir, slot+"_linux_"+arch)
}

// availableArchs lists the architectures for which a deployable binary exists
// for this slot. A plain `<slot>` (no arch suffix) counts as the server's own
// architecture.
func (s *Server) availableArchs(slot string) []string {
	if s.DeployDir == "" {
		return nil
	}
	got := []string{}
	for _, a := range deployArchs {
		if fileExists(s.archBinaryPath(slot, a)) {
			got = append(got, a)
		}
	}
	if len(got) == 0 && fileExists(filepath.Join(s.DeployDir, slot)) {
		got = append(got, runtime.GOARCH)
	}
	return got
}

// resolveDeployFile picks the on-disk binary for a slot and architecture.
// arch=="auto"/"" prefers the running server's own arch, then any available.
// An explicit (unknown/missing) arch yields "" so the caller reports a clear
// "architecture not available" error instead of silently serving a wrong arch.
func (s *Server) resolveDeployFile(slot, arch string) string {
	if s.DeployDir == "" {
		return ""
	}
	if arch == "" || arch == "auto" {
		visited := map[string]bool{}
		order := append([]string{runtime.GOARCH}, deployArchs...)
		for _, a := range order {
			if visited[a] {
				continue
			}
			visited[a] = true
			if p := s.archBinaryPath(slot, a); fileExists(p) {
				return p
			}
		}
		return filepath.Join(s.DeployDir, slot) // plain fallback (may not exist)
	}
	// explicit arch: must be a known one AND present, otherwise error (no fallback)
	if !containsStr(deployArchs, arch) {
		return ""
	}
	if p := s.archBinaryPath(slot, arch); fileExists(p) {
		return p
	}
	return ""
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (s *Server) handleDeployStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.GetConfig()
	server := ""
	if cfg != nil {
		server = cfg.PublicAddr
		if server == "" {
			server = cfg.Listen
		}
	}
	bins := map[string][]string{}
	all := map[string]bool{}
	for _, slot := range []string{"deepseek-client", "deepseek-natclient", "deepseek-server"} {
		bins[slot] = s.availableArchs(slot)
		for _, a := range bins[slot] {
			all[a] = true
		}
	}
	natIDs := []string{}
	if cfg != nil {
		for id := range cfg.Clients {
			natIDs = append(natIDs, id)
		}
	}
	archs := []string{}
	for _, a := range deployArchs {
		if all[a] {
			archs = append(archs, a)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":     s.DeployDir != "",
		"deploy_dir":  s.DeployDir,
		"server":      server,
		"archs":       archs,
		"server_arch": runtime.GOARCH,
		"binaries":    bins,
		"nat_ids":     natIDs,
	})
}

// ---- deploy tokens ----

func (s *Server) makeDeployToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	tok := randHex(b)
	s.deployMu.Lock()
	s.deployTok[tok] = time.Now().Add(deployTokenTTL)
	// prune expired periodically
	for t, exp := range s.deployTok {
		if time.Now().After(exp) {
			delete(s.deployTok, t)
		}
	}
	s.deployMu.Unlock()
	return tok
}

func (s *Server) deployTokenValid(tok string) bool {
	if tok == "" {
		return false
	}
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	exp, ok := s.deployTok[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.deployTok, tok)
		return false
	}
	return true
}

func randHex(b []byte) string {
	const hexc = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexc[v>>4]
		out[i*2+1] = hexc[v&0x0f]
	}
	return string(out)
}

func (s *Server) handleDeployToken(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"token": s.makeDeployToken(),
		"ttl":   deployTokenTTL.String(),
	})
}

// authorizes returns true if the request has a valid login session or a valid
// deploy bearer token.
func (s *Server) authorizes(r *http.Request) bool {
	if s.isAuthed(r) {
		return true
	}
	return s.deployTokenValid(bearerToken(r))
}

func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.Header.Get("X-Deploy-Token")
}

// handleDeployAsset serves binaries and generated configs under /api/deploy/.
// Authorized by session OR deploy token (so copy-pasted curl works remotely).
func (s *Server) handleDeployAsset(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/api/deploy/")
	if rel == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if rel == "status" || rel == "token" {
		// not handled here (exact routes handle them); 404 anyway
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if !s.authorizes(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "需要登录 或 使用部署令牌 (Authorization: Bearer <token>)"})
		return
	}

	// /api/deploy/cfg/<role>/<natid.json> or /api/deploy/cfg/<role>.json
	if strings.HasPrefix(rel, "cfg/") {
		s.serveDeployConfig(w, r, strings.TrimPrefix(rel, "cfg/"))
		return
	}
	// otherwise treat as a binary filename (deepseek-server/client/natclient)
	s.serveDeployBinary(w, r, rel)
}

func (s *Server) serveDeployConfig(w http.ResponseWriter, r *http.Request, tail string) {
	// tail is like "client.json" or "natclient/<id>.json"
	parts := strings.Split(tail, "/")
	role := strings.TrimSuffix(parts[0], ".json")
	suffix := ""
	if len(parts) > 1 {
		suffix = parts[1]
	}
	if role == "client" {
		b, err := s.buildClientConfig()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="client.json"`)
		w.Write(append(b, '\n'))
		return
	}
	if role == "natclient" {
		id := strings.TrimSuffix(suffix, ".json")
		b, err := s.buildNATConfig(id)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filepath.Base(id)+".json"))
		w.Write(append(b, '\n'))
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown config role " + role})
}

func (s *Server) serveDeployBinary(w http.ResponseWriter, r *http.Request, name string) {
	if strings.HasSuffix(name, ".json") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "config path is /api/deploy/cfg/..."})
		return
	}
	// whitelist: only the three known binaries are ever served (blocks `..`
	// path traversal out of deploy_dir via ?arch/name manipulation).
	if !containsStr([]string{"deepseek-server", "deepseek-client", "deepseek-natclient"}, name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "未知的二进制 " + name})
		return
	}
	arch := r.URL.Query().Get("arch")
	p := s.resolveDeployFile(name, arch)
	if p == "" || !fileExists(p) {
		avail := s.availableArchs(name)
		msg := fmt.Sprintf("二进制 %q (%s) 未找到；deploy_dir=%s", name, archOrAuto(arch), s.DeployDir)
		if len(avail) > 0 {
			msg += "；可用架构: " + strings.Join(avail, ", ")
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": msg})
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filepath.Base(p)))
	if st != nil {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", st.Size()))
	}
	_, _ = io.Copy(w, f)
}

func archOrAuto(a string) string {
	if a == "" {
		return "auto"
	}
	return a
}

// buildClientConfig returns a ready client.json (server per current config).
func (s *Server) buildClientConfig() ([]byte, error) {
	cfg := s.GetConfig()
	server := ""
	if cfg != nil {
		server = cfg.PublicAddr
		if server == "" {
			server = cfg.Listen
		}
	}
	doc := map[string]any{
		"server":                  server,
		"ca_file":                 "",
		"server_fingerprint":      "",
		"insecure_skip_verify":    false,
		"server_name":             "",
		"reconnect_delay_seconds": 5,
		"_comment":                "Set a TLS trust mode (ca_file or server_fingerprint) and, if needed, correct server host.",
	}
	return json.MarshalIndent(doc, "", "  ")
}

// buildNATConfig returns a ready natclient-<id>.json for the given client id.
func (s *Server) buildNATConfig(id string) ([]byte, error) {
	cfg := s.GetConfig()
	if cfg == nil {
		return nil, errors.New("no config")
	}
	nc, ok := cfg.Clients[id]
	if !ok {
		return nil, fmt.Errorf("unknown NAT client id %q", id)
	}
	server := cfg.PublicAddr
	if server == "" {
		server = cfg.Listen
	}
	doc := map[string]any{
		"server":  server,
		"ca_file": "", "server_fingerprint": "", "insecure_skip_verify": false,
		"client_id":               id,
		"secret":                  nc.Secret,
		"services":                nc.Services,
		"dial_timeout_seconds":    10,
		"reconnect_delay_seconds": 5,
		"_comment":                "Set a TLS trust mode.",
	}
	return json.MarshalIndent(doc, "", "  ")
}
