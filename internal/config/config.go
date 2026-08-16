// Package config holds the on-disk server configuration and load/save/validate
// helpers shared between the relay server and its web console.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// DefaultListenAddr is used as the relay+console (shared) listen address.
const DefaultListenAddr = ":8443"

// DefaultAdminListenAddr is used only as a fallback; a server config with an
// empty admin.listen means the console SHOULD share the relay port (8443).
const DefaultAdminListenAddr = ":8443"

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
	// LogFile is where the daemonized ('start') server writes its log.
	LogFile string `json:"log_file,omitempty"`
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
	if c.Admin == nil {
		c.Admin = &Admin{}
	}
	// Empty admin.listen => share the relay port (default :8443). We do NOT
	// fill it here so the server can detect "shared" vs "independent".
	if len(c.Clients) == 0 {
		c.Clients = map[string]*NatClient{}
	}
}

// Validate checks a config document (after defaults applied).
func (c *ServerConfig) Validate() error {
	if len(c.Routes) == 0 {
		return errors.New("'routes' must not be empty")
	}
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
