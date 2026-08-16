package main

// Rotating log output. When the server writes logs to a file (daemon `start`
// mode, or an explicit log_file in foreground/systemd mode), a size cap is
// enforced: once LogMaxBytes is exceeded the file is rotated (path -> .1 -> .2
// ...), keeping LogMaxFiles backups. This prevents unbounded disk growth.
//
// The rotator OWNS the underlying *os.File*, so log.SetOutput(w) plus
// os.Stdout/os.Stderr pointing at it route everything through rotation.

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

type rotatingLog struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	maxFiles int
	f        *os.File
	size     int64
}

// openLog rotates any oversized existing file, then opens path for append.
func openLog(path string, maxBytes int64, maxFiles int) (*rotatingLog, error) {
	l := &rotatingLog{path: path, maxBytes: maxBytes, maxFiles: maxFiles}
	if st, err := os.Stat(path); err == nil && st.Size() >= maxBytes && maxBytes > 0 {
		l.shiftFiles()
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	st, _ := f.Stat()
	if st != nil {
		l.size = st.Size()
	}
	l.f = f
	return l, nil
}

func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.maxBytes > 0 && l.size+int64(len(p)) > l.maxBytes {
		l.rotateLocked()
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *rotatingLog) rotateLocked() {
	l.f.Close()
	l.shiftFiles()
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		// keep going best-effort with the old descriptor; size still capped next write
		return
	}
	l.f = f
	l.size = 0
}

// shiftFiles renames path.(i) up one slot and path -> path.1, dropping extras.
func (l *rotatingLog) shiftFiles() {
	base := l.path
	for i := l.maxFiles - 1; i >= 1; i-- {
		from := base + "." + strconv.Itoa(i)
		to := base + "." + strconv.Itoa(i+1)
		_ = os.Rename(from, to)
	}
	_ = os.Rename(base, base+".1")
	// tidy stray higher numbers
	_ = os.Remove(base + "." + strconv.Itoa(l.maxFiles+1))
}

func (l *rotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		return l.f.Close()
	}
	return nil
}

// defaultLogDir returns the default log path (<config dir>/server.log) used
// when the `start` daemon has no explicit log_file configured.
func defaultLogDir(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), "server.log")
}
