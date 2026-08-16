package main

// CertManager reloads the server certificate/key whenever they change on disk,
// so ACME clients (e.g. certbot/acme.sh) that replace the files without notifying
// us are picked up automatically. On any reload error it keeps serving the last
// good certificate and retries on the next tick.

import (
	"crypto/tls"
	"log"
	"os"
	"sync"
	"time"
)

type CertManager struct {
	certFile string
	keyFile  string

	mu   sync.RWMutex
	cert *tls.Certificate
	// lastStat caches file mtimes to skip reloads when nothing changed.
	lastStat map[string]int64
}

// GetCertificate is wired into tls.Config and always returns the newest cert.
func (cm *CertManager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	if cm.cert == nil {
		return nil, errNoCert
	}
	return cm.cert, nil
}

var errNoCert = &noCertError{}

type noCertError struct{}

func (*noCertError) Error() string { return "tls: certificate not loaded yet" }

// Watch polls the cert/key files every interval and reloads on change.
func (cm *CertManager) Watch(interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	// Always attempt once first.
	cm.reload()
	for range time.Tick(interval) {
		if cm.changed() {
			cm.reload()
		}
	}
}

func (cm *CertManager) changed() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	for _, p := range []string{cm.certFile, cm.keyFile} {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		if cm.lastStat[p] != st.ModTime().UnixNano() {
			return true
		}
	}
	return false
}

func (cm *CertManager) reload() {
	cert, err := tls.LoadX509KeyPair(cm.certFile, cm.keyFile)
	if err != nil {
		log.Printf("WARN: cert reload failed (keeping previous): %v", err)
		return
	}
	cm.mu.Lock()
	cm.cert = &cert
	if cm.lastStat == nil {
		cm.lastStat = map[string]int64{}
	}
	for _, p := range []string{cm.certFile, cm.keyFile} {
		if st, err := os.Stat(p); err == nil {
			cm.lastStat[p] = st.ModTime().UnixNano()
		}
	}
	cm.mu.Unlock()
	log.Printf("certificate reloaded: %s", cm.certFile)
}
