package mux

import "encoding/json"

// PortMapping describes one listen port the client should bind locally and the
// target it maps to. It is sent from server to client inside the CONFIG frame.
type PortMapping struct {
	// Name is a human-readable label for logging (e.g. "deepseek").
	Name string `json:"name"`
	// Port is the local port the client should listen on.
	Port int `json:"port"`
	// Target is the server-side address (host:port) that connections on this
	// listen port are forwarded to.
	Target string `json:"target"`
}

// Config is the CONFIG frame payload sent by the server.
type Config struct {
	Version string        `json:"version"`
	Ports   []PortMapping `json:"ports"`
}

// EncodeConfig marshals a Config into the CONFIG frame payload.
func EncodeConfig(c Config) ([]byte, error) {
	return json.Marshal(c)
}

// DecodeConfig parses a CONFIG frame payload.
func DecodeConfig(b []byte) (Config, error) {
	var c Config
	err := json.Unmarshal(b, &c)
	return c, err
}

// Role values for Register.Role.
const (
	RoleEntry = "entry" // binds local ports, forwards to server
	RoleNat   = "nat"   // behind NAT, exposes local services via server
)

// ServiceInfo describes one locally-exposed service on a NAT client.
type ServiceInfo struct {
	// Name is a stable identifier referenced by server routes.
	Name string `json:"name"`
	// Port is the local listen port on the NAT host for this service.
	Port int `json:"port"`
	// Addr is the full local address to dial; defaults to "127.0.0.1:<Port>".
	Addr string `json:"addr,omitempty"`
}

// Register is the REGISTER frame payload sent by every client to the server
// immediately after the TLS handshake.
type Register struct {
	// Role is RoleEntry or RoleNat.
	Role string `json:"role"`
	// ClientID is the unique identity for NAT clients (and optional for entry).
	ClientID string `json:"client_id,omitempty"`
	// Secret authenticates NAT clients against the server's clients map.
	Secret string `json:"secret,omitempty"`
	// Services is the list of exposed services (NAT clients only).
	Services []ServiceInfo `json:"services,omitempty"`
}

// EncodeRegister marshals a Register into a REGISTER frame payload.
func EncodeRegister(r Register) ([]byte, error) { return json.Marshal(r) }

// DecodeRegister parses a REGISTER frame payload.
func DecodeRegister(b []byte) (Register, error) {
	var r Register
	err := json.Unmarshal(b, &r)
	return r, err
}
