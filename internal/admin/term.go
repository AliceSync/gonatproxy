package admin

// Simple shell terminal. A POST starts a command (bash -c) and streams its
// output over Server-Sent Events until it exits. Not a full PTY, but enough for
// most admin/systemctl/file commands.

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sync"
)

var (
	termLock    sync.Mutex
	currentProc *procRun
)

type procRun struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (s *Server) handleTermPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "term.html", map[string]any{"Root": s.FileRoot})
}

// handleTermRun streams command output via SSE, then emits a final 'done' event.
func (s *Server) handleTermRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "post only", http.StatusMethodNotAllowed)
		return
	}
	cmdline := r.URL.Query().Get("cmd")
	if cmdline == "" {
		http.Error(w, "missing cmd", http.StatusBadRequest)
		return
	}
	// only one command at a time
	termLock.Lock()
	if currentProc != nil {
		termLock.Unlock()
		http.Error(w, "another command is running; wait for it to finish", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", cmdline)
	cmd.Dir = s.FileRoot
	cmd.Env = append(cmd.Environ(), "LC_ALL=C")
	stdout, err1 := cmd.StdoutPipe()
	stderr, err2 := cmd.StderrPipe()
	if err1 != nil || err2 != nil {
		cancel()
		termLock.Unlock()
		http.Error(w, "pipe error", http.StatusInternalServerError)
		return
	}
	if err := cmd.Start(); err != nil {
		cancel()
		termLock.Unlock()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pr := &procRun{cmd: cmd, done: make(chan struct{})}
	currentProc = pr
	termLock.Unlock()

	// SSE setup
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)

	send := func(name, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, encB64(data))
		if fl != nil {
			fl.Flush()
		}
	}

	go func() { _ = copyPipe(stdout, send) }()
	go func() { _ = copyPipe(stderr, send) }()
	go func() { _ = cmd.Wait(); close(pr.done) }()

	select {
	case <-pr.done:
	case <-r.Context().Done():
		_ = cmd.Process.Kill()
	}

	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	termLock.Lock()
	if currentProc == pr {
		currentProc = nil
	}
	termLock.Unlock()
	cancel()
	fmt.Fprintf(w, "event: done\ndata: %d\n\n", exitCode)
	if fl != nil {
		fl.Flush()
	}
}

func encB64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func copyPipe(src io.Reader, send func(string, string)) error {
	buf := make([]byte, 4096)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			send("out", string(buf[:n]))
		}
		if err != nil {
			return err
		}
	}
}
