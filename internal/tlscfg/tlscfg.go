// Package tlscfg builds *tls.Config for outbound clients with three trust
// modes, selected via the fields below:
//
//   - fingerprint:     verify the server's leaf certificate against a SHA-256
//     hex fingerprint (most useful when you cannot pin the CA because certs
//     rotate, e.g. ACME issuance).
//   - insecure:        skip verification entirely (insecure; discouraged).
//   - default:         verify against the CA in CAFile (the recommended,
//     "trusted" mode).
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

// Client holds outbound TLS trust configuration. Exactly one behaviour is
// used; the precedence is fingerprint > insecure > CA.
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

	// Default: verify against a CA (trusted).
	if c.CAFile == "" {
		return nil, fmt.Errorf("tlscfg: nothing to trust — set ca_file, server_fingerprint, or insecure_skip_verify")
	}
	pool, err := loadPool(c.CAFile)
	if err != nil {
		return nil, err
	}
	cfg.RootCAs = pool
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
