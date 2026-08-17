// Package tlscfg builds *tls.Config for outbound clients.
// Trust precedence:
//
//   - fingerprint:        verify the peer leaf certificate against a SHA-256
//     hex fingerprint (most useful when you cannot pin the CA because certs
//     rotate, e.g. ACME issuance).
//   - insecure:           skip verification entirely (insecure; discouraged).
//   - ca_file:            verify against a custom CA bundle (private CA).
//   - default (nothing):  normal TLS verification against the system's
//     built-in CA store — no ca_file needed for publicly-trusted certs.
package tlscfg

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// Client holds outbound TLS trust configuration. Precedence:
// fingerprint > insecure > CAFile(/system default).
type Client struct {
	// CAFile is a PEM CA certificate used to verify the server's cert.
	CAFile string
	// ServerFingerprint is the hex(sha256) of the server's leaf certificate.
	ServerFingerprint string
	// InsecureSkipVerify disables verification.
	InsecureSkipVerify bool
	// ServerName overrides verification hostname/SNI.
	ServerName string
}

// Build produces a *tls.Config for the given peer address (host:port).
//
// Precedence:
//  1. server_fingerprint — verify the peer leaf against this hex(sha256).
//  2. insecure_skip_verify — disable verification entirely.
//  3. ca_file — verify against a custom CA bundle (private CA).
//  4. (default) — normal TLS verification against the system's own built-in
//     CA store (the OS already ships trusted public roots; no ca_file needed).
func (c Client) Build() (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: c.ServerName,
	}

	if c.ServerFingerprint != "" {
		fp := strings.ToLower(strings.ReplaceAll(c.ServerFingerprint, ":", ""))
		cfg.InsecureSkipVerify = true
		cfg.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return fmt.Errorf("tls: no peer certificate to fingerprint")
			}
			sum := sha256.Sum256(raw[0])
			got := hex.EncodeToString(sum[:])
			if got != fp {
				return fmt.Errorf("tls: certificate fingerprint mismatch (got %s)", got)
			}
			return nil
		}
		return cfg, nil
	}

	if c.InsecureSkipVerify {
		cfg.InsecureSkipVerify = true // #nosec G402 -- explicit user choice
		return cfg, nil
	}

	// Default: normal verification against system roots. A custom ca_file (e.g.
	// a private/internal CA) overrides the root pool; otherwise the local OS
	// trust store's built-in public certificates are used.
	if c.CAFile != "" {
		pool, err := loadPool(c.CAFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

func loadPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("tlscfg: could not parse CA file %s", path)
	}
	return pool, nil
}
