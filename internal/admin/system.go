package admin

// Services + system update/restart handlers. These shell out to systemctl
// (when systemd is present) and implement binary update + graceful restart via
// host-provided Hooks.

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// systemdActive reports whether the console is running under a live systemd.
func systemdActive() bool {
	if os.Getenv("INVOCATION_ID") != "" {
		return true
	}
	if fileInfoExists("/run/systemd/system") {
		return true
	}
	return false
}

func fileInfoExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (s *Server) handleServicesPage(w http.ResponseWriter, r *http.Request) {
	inspect := s.inspectService()
	s.render(w, "services.html", map[string]any{
		"Unit":      inspect.Unit,
		"Config":    s.GetConfig(),
		"IsSystemd": inspect.Systemd,
		"Installed": inspect.Installed,
		"Daemon":    inspect.Daemon,
	})
}

func (s *Server) inspectService() ServiceInspect {
	if h := s.serviceHooks(); h != nil && h.Inspect != nil {
		return h.Inspect()
	}
	return ServiceInspect{Unit: s.hooks().UnitIfAny()}
}

func (s *Server) serviceHooks() *ServiceHooks {
	if s.Hooks != nil && s.Hooks.Service != nil {
		return s.Hooks.Service
	}
	return nil
}

// handleServiceInspect returns the current services/systemd state as JSON.
func (s *Server) handleServiceInspect(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.inspectService())
}

// handleServiceGenerate writes the systemd unit file + daemon-reload.
func (s *Server) handleServiceGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "post only", http.StatusMethodNotAllowed)
		return
	}
	h := s.serviceHooks()
	if h == nil || h.Generate == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "service generation not available in this build"})
		return
	}
	res, err := h.Generate()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleServiceControl runs enable|disable|start|stop|restart|daemon-reload.
func (s *Server) handleServiceControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "post only", http.StatusMethodNotAllowed)
		return
	}
	h := s.serviceHooks()
	if h == nil || h.Control == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "service control not available in this build"})
		return
	}
	action := r.URL.Query().Get("action")
	if !validServiceAction(action) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid action"})
		return
	}
	res, err := h.Control(action)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func validServiceAction(a string) bool {
	switch a {
	case "enable", "disable", "start", "stop", "restart", "daemon-reload", "status":
		return true
	}
	return false
}

func (s *Server) handleSystemPage(w http.ResponseWriter, r *http.Request) {
	h := s.hooks()
	version := ""
	if h != nil && h.Version != nil {
		version = h.Version()
	}
	var staged bool
	if h != nil && h.StageDir != "" {
		staged = fileExists(filepath.Join(h.StageDir, stagedName))
	}
	s.render(w, "system.html", map[string]any{
		"Version":   version,
		"Staged":    staged,
		"IsSystemd": systemdActive(),
		"Unit":      h.UnitIfAny(),
	})
}

func (s *Server) hooks() *SystemHooks { return s.Hooks }

func (s *SystemHooks) UnitIfAny() string {
	if s == nil {
		return ""
	}
	return s.SystemdUnit
}

// handleSystemctl runs a systemctl subcommand (enable/disable/start/stop/
// restart/status) for a unit. enable and start are kept separate.
func (s *Server) handleSystemctl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "post only", 405)
		return
	}
	action := r.URL.Query().Get("action")
	unit := r.URL.Query().Get("unit")
	if !validAction(action) || unit == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid action/unit"})
		return
	}
	if !systemdActive() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "systemd is not running; use ./scripts/install.sh or manual commands"})
		return
	}
	cmd := exec.Command("systemctl", action, unit)
	out, err := cmd.CombinedOutput()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": string(out)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": strings.TrimSpace(string(out))})
}

func validAction(a string) bool {
	switch a {
	case "enable", "disable", "start", "stop", "restart", "daemon-reload", "status":
		return true
	}
	return false
}

// ---- update & restart ----

const stagedName = "deepseek-server.new"

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	h := s.hooks()
	if h == nil || h.StageDir == "" || h.ApplyUpdate == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "update not available in this build"})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "post only", 405)
		return
	}
	if err := r.ParseMultipartForm(128 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// user must explicitly confirm restart
	restartNow := r.FormValue("restart") == "1"
	confirm := r.FormValue("apply") == "1"

	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no file uploaded"})
		return
	}
	src, err := files[0].Open()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer src.Close()
	stage := filepath.Join(h.StageDir, stagedName)
	if err := copyToFile(stage, src, files[0].Size); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	note := "二进制已上传（暂存）。"
	if restartNow {
		if !confirm {
			writeJSON(w, http.StatusOK, map[string]string{"ok": "已上传并暂存，但未应用（未确认重启）", "staged": "1"})
			return
		}
		msg, err := h.ApplyUpdate(stage, true)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ok": "已应用并触发重启: " + msg, "restarting": "1"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": note + " 在系统页点击【应用并重启】即可生效。", "staged": "1"})
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "post only", 405)
		return
	}
	if h := s.hooks(); h != nil && h.Restart != nil {
		if err := h.Restart(h.Grace); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		// if Restart returns (e.g. async), tell client
		writeJSON(w, http.StatusOK, map[string]string{"restarting": "1"})
		return
	}
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "restart not available"})
}

// handleApplyStaged applies a previously staged update binary (no upload needed)
// and then performs a graceful restart.
func (s *Server) handleApplyStaged(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "post only", http.StatusMethodNotAllowed)
		return
	}
	h := s.hooks()
	if h == nil || h.StageDir == "" || h.ApplyUpdate == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "update not available"})
		return
	}
	stage := filepath.Join(h.StageDir, stagedName)
	if !fileExists(stage) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "没有暂存的更新文件"})
		return
	}
	msg, err := h.ApplyUpdate(stage, true)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "已应用并重启: " + msg, "restarting": "1"})
}

func (s *Server) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	h := s.hooks()
	version := ""
	if h != nil && h.Version != nil {
		version = h.Version()
	}
	inspect := s.inspectService()
	staged := "0"
	if h != nil && h.StageDir != "" {
		staged = boolStr(fileExists(filepath.Join(h.StageDir, stagedName)))
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"version":  version,
		"exe":      inspect.Exe,
		"config":   s.ConfigPath,
		"systemd":  boolStr(inspect.Systemd),
		"managed":  boolStr(inspect.Managed),
		"daemon":   boolStr(inspect.Daemon),
		"unit":     inspect.Unit,
		"run_user": inspect.RunUser,
		"staged":   staged,
	})
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
