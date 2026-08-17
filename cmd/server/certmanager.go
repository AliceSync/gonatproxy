package main

// CertManager supplies the server's TLS certificate. If cert_file/key_file are
// configured and exist they are used and watched so ACME renewals are picked up
// automatically; otherwise an ephemeral self-signed certificate is generated
// in memory (useful in initialization mode when nothing is configured yet).

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log"
	"math/big"
	"net"
	"os"
	"sync"
	"time"
)

type CertManager struct {
	certFile string
	keyFile  string

	mu        sync.RWMutex
	cert      *tls.Certificate
	ephemeral bool
}

func newCertManager(certFile, keyFile string) *CertManager {
	return &CertManager{certFile: certFile, keyFile: keyFile}
}

// paths returns the current cert/key paths (safe to call from anywhere).
func (cm *CertManager) paths() (string, string) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.certFile, cm.keyFile
}

// Update switches the manager to a new cert_file/key_file (e.g. after the admin
// edits the configuration) and immediately (re)loads the certificate — either
// the new on-disk pair, or an ephemeral one if the paths are empty/missing.
// This makes config edits take effect without restarting the process.
func (cm *CertManager) Update(certFile, keyFile string) {
	curC, curK := cm.paths()
	if curC == certFile && curK == keyFile {
		return // unchanged
	}
	cm.mu.Lock()
	cm.certFile = certFile
	cm.keyFile = keyFile
	cm.mu.Unlock()
	log.Printf("cert manager: configured cert_file=%q key_file=%q", certFile, keyFile)
	cm.Ensure()
}

// GetCertificate is wired into tls.Config and returns the current cert.
func (cm *CertManager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.cert, nil
}

// IsEphemeral reports whether the current cert was generated in memory.
func (cm *CertManager) IsEphemeral() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.ephemeral
}

func (cm *CertManager) filesAvailable() bool {
	certFile, keyFile := cm.paths()
	if certFile == "" || keyFile == "" {
		return false
	}
	_, e1 := os.Stat(certFile)
	_, e2 := os.Stat(keyFile)
	return e1 == nil && e2 == nil
}

// Ensure loads cert files if present, otherwise generates an ephemeral cert.
func (cm *CertManager) Ensure() {
	if cm.filesAvailable() {
		cm.reload()
		return
	}
	cm.genEphemeral()
}

// Watch polls the cert files so ACME renewals are picked up automatically.
func (cm *CertManager) Watch(interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	cm.Ensure()
	for range time.Tick(interval) {
		if cm.filesAvailable() {
			cm.reload()
		}
	}
}

func (cm *CertManager) reload() {
	certFile, keyFile := cm.paths()
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		log.Printf("WARN: cert reload failed (keeping previous): %v", err)
		return
	}
	cm.mu.Lock()
	cm.cert = &cert
	cm.ephemeral = false
	cm.mu.Unlock()
	log.Printf("certificate loaded/reloaded: %s", certFile)
}

func (cm *CertManager) genEphemeral() {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatalf("generate ephemeral key: %v", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()),
		Subject:      pkix.Name{CommonName: "deepseek-tunnel-ephemeral"},
		NotBefore:    now.Add(-10 * time.Minute),
		NotAfter:     now.AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost", "deepseek.local"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		log.Fatalf("create ephemeral cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		log.Fatalf("marshal ephemeral key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		log.Fatalf("load ephemeral keypair: %v", err)
	}
	cm.mu.Lock()
	cm.cert = &cert
	cm.ephemeral = true
	cm.mu.Unlock()
	log.Printf("using ephemeral self-signed TLS certificate (in memory, temporary); configure cert_file/key_file (e.g. ACME) to persist")
}
