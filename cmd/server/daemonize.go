package main

// daemonize re-executes the server detached from the terminal so it runs in the
// background, logging to the configured log file (or a sensible default).

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"deepseekaiworker/internal/config"
)

// resolveLogFile picks where a daemonized server logs.
func resolveLogFile(cfg *config.ServerConfig, cfgPath string, missing bool) string {
	if cfg != nil && cfg.LogFile != "" {
		return cfg.LogFile
	}
	// default: <config dir>/server.log
	dir := filepath.Dir(cfgPath)
	return filepath.Join(dir, "server.log")
}

// daemonize forks a background child running "run" with the given config and
// exits the parent. When config is missing, the child runs in init mode (the
// web console), which is exactly what we want for first-time setup.
func daemonize(cfgPath string, cfg *config.ServerConfig, missing bool) {
	exe, err := os.Executable()
	if err != nil {
		exe = "/usr/local/bin/deepseek-server"
	}

	logFile := resolveLogFile(cfg, cfgPath, missing)
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatalf("daemonize: cannot open log file %s: %v", logFile, err)
	}
	// ensure our own startup messages go to the daemon log too
	log.SetOutput(f)

	cmd := exec.Command(exe, "run", "-config", cfgPath)
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(), "DEEPSEEK_DAEMON=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		log.Fatalf("daemonize: start: %v", err)
	}
	fmt.Printf("deepseek-server running in background: pid=%d log=%s\n", cmd.Process.Pid, logFile)
	fmt.Printf("web console available at http://127.0.0.1:8444 (or per config)\n")

	// Detach: release the child from this process group without waiting.
	_ = cmd.Process.Release()
}
