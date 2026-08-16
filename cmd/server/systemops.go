package main

// Graceful shutdown, self-fork restart and binary-update hooks used by the web
// console (系统 page). Restart is separate from going-to-background:
// - Restart: stop the accept loop (grace), fork a replacement process with the
//   same args, then exit. This powers "立即重启" and "应用更新后重启".
// - Update : move a staged binary over the running binary, then (optionally)
//   restart. mv-replace + fork start.

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"deepseekaiworker/internal/admin"
)

var (
	stopMu sync.Mutex
	stops  []func()
)

// registerStop adds a closer invoked on graceful restart (to free ports).
func registerStop(f func()) {
	stopMu.Lock()
	stops = append(stops, f)
	stopMu.Unlock()
}

// stopAll runs every registered closer once.
func stopAll() {
	stopMu.Lock()
	ops := stops
	stops = nil
	stopMu.Unlock()
	for _, f := range ops {
		f()
	}
}

// buildAdminHooks supplies the console's privileged operations. stop is unused
// (kept for compatibility); port freeing happens via registerStop.
func buildAdminHooks(cfgPath string) *admin.SystemHooks {
	exe, _ := os.Executable()
	args := os.Args[1:]
	h := &admin.SystemHooks{
		Version:     func() string { return version },
		Grace:       3 * time.Second,
		StageDir:    os.Getenv("DEEPSEEK_STAGE_DIR"),
		SystemdUnit: os.Getenv("DEEPSEEK_SYSTEMD_UNIT"),
	}
	if h.StageDir == "" {
		h.StageDir = filepath.Dir(exe)
	}
	if h.SystemdUnit == "" {
		h.SystemdUnit = detectSystemdUnit()
	}

	h.Restart = func(grace time.Duration) error {
		if grace <= 0 {
			grace = 3 * time.Second
		}
		// Under systemd, restarting should go through systemd so it keeps owning
		// the unit; forking our own copy would orphan a second instance.
		if systemdOwnsUs() {
			log.Printf("restart requested; delegating to systemctl restart %s", serviceUnitName())
			if out, err := sysctlOutput("restart", serviceUnitName()); err != nil {
				return fmt.Errorf("systemctl restart: %s", out)
			}
			return nil
		}
		log.Printf("restart requested; closing listeners, draining %s", grace)
		stopAll()
		time.Sleep(grace)
		if err := forkSelf(exe, args); err != nil {
			log.Printf("restart fork failed: %v", err)
			return err
		}
		log.Println("restart: forked replacement; exiting")
		go func() { time.Sleep(300 * time.Millisecond); os.Exit(0) }()
		return nil
	}

	h.ApplyUpdate = func(stagedPath string, restartNow bool) (string, error) {
		return applyBinary(exe, stagedPath, restartNow, h.Restart)
	}

	h.Service = &admin.ServiceHooks{
		Inspect:  func() admin.ServiceInspect { return serviceInspect(cfgPath) },
		Generate: serviceGenerateSafe(cfgPath),
		Control:  func(action string) (admin.ServiceResult, error) { return controlService(cfgPath, action) },
	}
	return h
}

// serviceGenerateSafe wraps installServiceUnit so a privilege error surfaces as
// a clear message rather than a stack trace.
func serviceGenerateSafe(cfgPath string) func() (admin.ServiceResult, error) {
	return func() (admin.ServiceResult, error) {
		path, err := installServiceUnit(cfgPath)
		if err != nil {
			return admin.ServiceResult{}, err
		}
		return admin.ServiceResult{OK: "已写入并 daemon-reload：" + path}, nil
	}
}

func detectSystemdUnit() string {
	if u := os.Getenv("DEEPSEEK_SYSTEMD_UNIT"); u != "" {
		return u
	}
	return "" // we don't reliably know a unit name otherwise
}

// forkSelf launches a replacement of exe with args, detached.
func forkSelf(exe string, args []string) error {
	cmd := exec.Command(exe, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "DEEPSEEK_RESTARTED=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Printf("fork-restart started pid=%d", cmd.Process.Pid)
	return cmd.Process.Release()
}

// applyBinary moves the staged binary over the running binary (mv-replace),
// optionally restarting. Returns a note.
func applyBinary(exe, stagedPath string, restartNow bool, restart func(time.Duration) error) (string, error) {
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		real = exe
	}
	if err := os.Chmod(stagedPath, 0o755); err != nil {
		return "", err
	}
	old := real + ".old"
	_ = os.Remove(old)
	if _, err := os.Stat(real); err == nil {
		_ = os.Rename(real, old)
	}
	if err := os.Rename(stagedPath, real); err != nil {
		_ = os.Rename(old, real)
		return "", err
	}
	note := "已替换程序（旧版本备份为 " + old + "）"
	if restartNow {
		if err := restart(3 * time.Second); err != nil {
			return note + "；重启失败：" + err.Error(), err
		}
		note += "，并已触发重启"
	}
	return note, nil
}
