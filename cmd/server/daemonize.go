package main

// daemonize re-executes the server detached from the terminal so it runs in the
// background. Logging is done by `run` via a self-rotating log file; here we
// just resolve the log path/settings and pass them to the child through env,
// then return. The child writes to (and rotates) the resolved log file.

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"deepseekaiworker/internal/config"
)

// resolveLogFile picks where a daemonized server logs: the configured log_file,
// else the default <config dir>/server.log (which supports rotation).
func resolveLogFile(cfg *config.ServerConfig, cfgPath string) string {
	if cfg != nil && cfg.LogFile != "" {
		return cfg.LogFile
	}
	return defaultLogDir(cfgPath)
}

func daemonize(cfgPath string, cfg *config.ServerConfig, missing bool) {
	exe, err := os.Executable()
	if err != nil {
		exe = "/usr/local/bin/deepseek-server"
	}

	logFile := resolveLogFile(cfg, cfgPath)
	maxBytes, maxFiles := int64(100<<20), 5
	if cfg != nil {
		maxBytes, maxFiles = cfg.LogMaxBytes, cfg.LogMaxFiles
	}

	// Child will own + rotate the log file itself. Bypass the parent TTY so the
	// daemon detaches cleanly; run() re-routes stdout/stderr to the rotator.
	cmd := exec.Command(exe, "run", "-config", cfgPath)
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(),
		"DEEPSEEK_DAEMON=1",
		"DEEPSEEK_LOG_FILE="+logFile,
		fmt.Sprintf("DEEPSEEK_LOG_MAX=%d", maxBytes),
		fmt.Sprintf("DEEPSEEK_LOG_FILES=%d", maxFiles),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		log.Fatalf("daemonize: start: %v", err)
	}
	fmt.Printf("deepseek-server running in background: pid=%d log=%s\n", cmd.Process.Pid, logFile)
	fmt.Printf("web console available at https://%s (or per config)\n", cfgListen(cfg))

	// Detach: release the child from this process group without waiting.
	_ = cmd.Process.Release()
}

// cfgListen returns a usable host:port for the console, defaulting the host to
// 127.0.0.1 (never a bare ':port').
func cfgListen(cfg *config.ServerConfig) string {
	addr := ""
	if cfg != nil && cfg.Admin != nil {
		addr = cfg.Admin.Listen
	}
	if addr == "" {
		if cfg != nil && cfg.Listen != "" {
			addr = cfg.Listen
		} else {
			addr = ":8443"
		}
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return strings.TrimPrefix(addr, ":")
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
