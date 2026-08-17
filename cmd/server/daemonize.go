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
	// Foreground pre-flight: if any port the child will bind is already in use,
	// fail NOW (exit non-zero) instead of backgrounding a process that can't
	// bind and then silently dying — that's what produces the confusion.
	if err := preflightListen(cfg); err != nil {
		log.Fatalf("start 前置检查失败，%v", err)
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
	relay, console := announceAddrs(cfg)
	fmt.Printf("deepseek-server running in background: pid=%d log=%s\n", cmd.Process.Pid, logFile)
	fmt.Printf("relay listen : %s\n", relay)
	fmt.Printf("web console  : https://%s\n", console)
	if local := localConsoleURL(console); local != "" {
		fmt.Printf("  local access: https://%s\n", local)
	} else {
		fmt.Printf("  (console shares the relay port; https://%s)\n", console)
	}

	// Detach: release the child from this process group without waiting.
	_ = cmd.Process.Release()
}

// announceAddrs returns the configured (relay, console) host:port to display
// BEFORE the process switches to the background. It reflects the config verbatim,
// never collapsing a wildcard bind down to 127.0.0.1.
func announceAddrs(cfg *config.ServerConfig) (string, string) {
	relay := config.DefaultListenAddr
	if cfg != nil && cfg.Listen != "" {
		relay = cfg.Listen
	}
	console := relay
	if cfg != nil && cfg.Admin != nil && cfg.Admin.Listen != "" {
		console = cfg.Admin.Listen
	}
	return normalizeWildcard(relay), normalizeWildcard(console)
}

// localConsoleURL returns a 127.0.0.1 URL when console binds all interfaces
// (a catch-all wildcard host), else "".
func localConsoleURL(console string) string {
	host, port, err := net.SplitHostPort(console)
	if err != nil {
		return ""
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return net.JoinHostPort("127.0.0.1", port)
	}
	return ""
}

// normalizeWildcard turns an empty/wildcard host into "0.0.0.0" so the printed
// bind address is explicit and correct.
func normalizeWildcard(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return net.JoinHostPort("0.0.0.0", port)
	}
	return net.JoinHostPort(host, port)
}

// preflightListen verifies that every address the child will bind is free. It
// binds each once (then closes) so an occupied port is reported in the
// FOREGROUND with a clear error + non-zero exit, long before switching to the
// background. No random/ephemeral port fallback is used anywhere.
func preflightListen(cfg *config.ServerConfig) error {
	relay := config.DefaultListenAddr
	if cfg != nil && cfg.Listen != "" {
		relay = cfg.Listen
	}
	want := map[string]bool{relay: true}
	if cfg != nil && cfg.Admin != nil && cfg.Admin.Listen != "" && !sameAddr(cfg.Admin.Listen, relay) {
		// independent console port is also held by the child
		want[cfg.Admin.Listen] = true
	}
	for addr := range want {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("端口 %s 被占用，无法后台启动", addr)
		}
		l.Close()
	}
	return nil
}
