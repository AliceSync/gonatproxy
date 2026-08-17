// Package config holds the on-disk server configuration and load/save/validate
// helpers shared between the relay server and its web console.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// DefaultListenAddr is used as the relay+console (shared) listen address when
// the config leaves `listen` unset. 0.0.0.0 = all network interfaces.
const DefaultListenAddr = "0.0.0.0:8443"

// DefaultAdminListenAddr is used only in messages; an empty admin.listen means
// the console SHOULD share the relay port (which defaults to 0.0.0.0:8443).
const DefaultAdminListenAddr = "0.0.0.0:8443"

// Service describes one locally-exposed service on a NAT client (for export and
// display; live routing uses what the NAT client reports at registration time).
type Service struct {
	Name string `json:"name"`
	Port int    `json:"port"`
	Addr string `json:"addr,omitempty"`
}

// NatClient is one registered NAT client definition (secret + intended services).
type NatClient struct {
	Secret   string    `json:"secret"`
	Services []Service `json:"services,omitempty"`
}

// Route is one forwarding rule visible to entry clients via a listen port.
// Exactly one of Target or (NatClient+Service) must be set.
type Route struct {
	Name       string `json:"name"`
	ListenPort int    `json:"listen_port"`
	Target     string `json:"target,omitempty"`
	NatClient  string `json:"nat_client,omitempty"`
	Service    string `json:"service,omitempty"`
}

// Admin is the web console configuration.
type Admin struct {
	// Listen is the console HTTP(S) listen address.
	Listen string `json:"listen"`
	// TLS, when true, serves the console over TLS using CertFile/KeyFile.
	TLS      bool   `json:"tls"`
	CertFile string `json:"cert_file,omitempty"`
	KeyFile  string `json:"key_file,omitempty"`
	// Username/PasswordHash gate login. When either is empty the console runs
	// in initialization mode (no auth, forces account creation).
	Username     string `json:"username,omitempty"`
	PasswordHash string `json:"password_hash,omitempty"`
}

// ServerConfig is the root server configuration document.
type ServerConfig struct {
	Listen             string `json:"listen"`
	CertFile           string `json:"cert_file"`
	KeyFile            string `json:"key_file"`
	CertCheckSeconds   int    `json:"cert_check_seconds"`
	DialTimeoutSeconds int    `json:"dial_timeout_seconds"`
	// LogFile is where the daemonized ('start') server writes its log. May be
	// left empty: when running under systemd (ExecStart in foreground) logs go
	// to the journal, and a `start` daemon falls back to <config dir>/server.log.
	LogFile string `json:"log_file,omitempty"`
	// LogMaxBytes limits how large LogFile may grow before it is rotated.
	// 0 or negative => default 100 MiB. Rotation keeps LogMaxFiles backups.
	LogMaxBytes int64 `json:"log_max_bytes,omitempty"`
	// LogMaxFiles is how many rotated log backups to keep (0 => default 5).
	LogMaxFiles int `json:"log_max_files,omitempty"`
	// DeployDir is where the compiled binaries (deepseek-server/client/
	// natclient) live, used by the console 部署助手 to hand binaries to
	// entry/NAT hosts. Empty => auto-detect next to the running server binary.
	DeployDir string `json:"deploy_dir,omitempty"`
	// PublicAddr is the server's publicly reachable address (host:port) used
	// when generating entry/NAT client configs for export.
	PublicAddr string `json:"public_addr,omitempty"`

	Clients map[string]*NatClient `json:"clients"`
	Routes  []Route               `json:"routes"`
	Admin   *Admin                `json:"admin,omitempty"`
}

// Load reads and parses a config file. Returns os.ErrNotExist-style missing
// error when the file does not exist (caller should start in init mode).
func Load(path string) (*ServerConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c ServerConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.Normalize()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return &c, nil
}

// Normalize fills in defaults for any unset fields.
func (c *ServerConfig) Normalize() {
	if c.Listen == "" {
		c.Listen = DefaultListenAddr
	}
	if c.CertCheckSeconds <= 0 {
		c.CertCheckSeconds = 30
	}
	if c.DialTimeoutSeconds <= 0 {
		c.DialTimeoutSeconds = 10
	}
	if c.LogMaxBytes <= 0 {
		c.LogMaxBytes = 100 << 20 // 100 MiB
	}
	if c.LogMaxFiles <= 0 {
		c.LogMaxFiles = 5
	}
	if c.LogMaxFiles > 50 {
		c.LogMaxFiles = 50
	}
	if c.Admin == nil {
		c.Admin = &Admin{}
	}
	// Empty admin.listen => share the relay port (default :8443). We do NOT
	// fill it here so the server can detect "shared" vs "independent".
	if len(c.Clients) == 0 {
		c.Clients = map[string]*NatClient{}
	}
}

// Validate checks a config document (after defaults applied). Empty routes are
// allowed: the server then runs only the web console (relay disabled), so the
// admin can finish setup and add routes later without a fatal error.
func (c *ServerConfig) Validate() error {
	seen := map[int]string{}
	for _, r := range c.Routes {
		if r.Name == "" {
			return errors.New("route with empty 'name'")
		}
		if r.ListenPort <= 0 || r.ListenPort > 65535 {
			return fmt.Errorf("route %q: invalid listen_port", r.Name)
		}
		if prev, dup := seen[r.ListenPort]; dup {
			return fmt.Errorf("duplicate listen_port %d (routes %q and %q)", r.ListenPort, prev, r.Name)
		}
		seen[r.ListenPort] = r.Name
		if (r.Target == "") == (r.NatClient == "") {
			return fmt.Errorf("route %q: set exactly one of 'target' or 'nat_client'+'service'", r.Name)
		}
		if r.NatClient != "" && !c.hasClient(r.NatClient) {
			return fmt.Errorf("route %q references undefined nat_client %q", r.Name, r.NatClient)
		}
	}
	return nil
}

func (c *ServerConfig) hasClient(id string) bool {
	_, ok := c.Clients[id]
	return ok
}

// Save writes the config as indented JSON.
func (c *ServerConfig) Save(path string) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// NatClientView is a serializable NAT client config for the export endpoint.
type NatClientView struct {
	Server                string    `json:"server"`
	ClientID              string    `json:"client_id"`
	Secret                string    `json:"secret"`
	ReconnectDelaySeconds int       `json:"reconnect_delay_seconds"`
	DialTimeoutSeconds    int       `json:"dial_timeout_seconds"`
	Services              []Service `json:"services"`
}
