// Package config loads and validates the mcp-bridge YAML configuration file.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration structure parsed from config.yaml.
type Config struct {
	// Server contains wrapper-level settings (listen address, log level, …).
	Server ServerSettings `yaml:"server"`

	// Servers lists each MCP server to manage (stdio or network).
	Servers []ServerConfig `yaml:"servers"`
}

// ServerSettings controls the wrapper's own HTTP listener and logging.
type ServerSettings struct {
	// DataDir is the directory used to store persistent data (TLS certificates,
	// etc.). Defaults to "./.mcp-bridge".
	DataDir string `yaml:"data_dir"`

	// Addr is the TCP address the HTTP MCP endpoint listens on.
	// Defaults to ":7575".
	Addr string `yaml:"addr"`

	// Path is the HTTP path for the MCP endpoint.
	// Defaults to "/mcp".
	Path string `yaml:"path"`

	// LogLevel controls the minimum log severity: debug, info, warn, error.
	// Defaults to "info".
	LogLevel string `yaml:"log_level"`

	// AuthToken is an optional Bearer token required on every request.
	// When empty, authentication is disabled and any client may connect.
	// Clients must send:  Authorization: Bearer <token>
	AuthToken string `yaml:"auth_token"`

	// TLS configures HTTPS. When absent or all fields are empty, the server
	// listens over plain HTTP.
	TLS TLSSettings `yaml:"tls"`
}

// TLSSettings controls HTTPS for the MCP endpoint.
// Three modes are supported:
//
//  1. Disabled (default) — all fields empty; plain HTTP is used.
//  2. Custom certificates — set cert_file and key_file to paths of
//     PEM-encoded certificate and private key files.
//  3. Auto-generated self-signed certificate — set auto_cert: true.
//     A new ECDSA-P256 cert is generated in memory at startup.
//     Useful for development or internal networks.
type TLSSettings struct {
	// CertFile is the path to a PEM-encoded TLS certificate file.
	CertFile string `yaml:"cert_file"`

	// KeyFile is the path to a PEM-encoded private key file.
	KeyFile string `yaml:"key_file"`

	// AutoCert generates a self-signed certificate in memory at startup.
	// Ignored when CertFile/KeyFile are set.
	AutoCert bool `yaml:"auto_cert"`
}

// ServerConfig describes one MCP server — either a local stdio binary or a
// remote network server. Exactly one of Command or URL must be set.
type ServerConfig struct {
	// Name is used as the tool-name prefix (e.g. "git" → "git_status").
	// Must be unique across all servers. Must not contain underscores.
	Name string `yaml:"name"`

	// --- stdio mode (Command must be set, URL must be empty) ---

	// Command is the absolute (or PATH-resolvable) path to the binary.
	Command string `yaml:"command"`

	// Args are optional command-line arguments forwarded to the binary.
	Args []string `yaml:"args"`

	// Env contains optional KEY=VALUE pairs injected into the child environment.
	// The child always inherits the bridge's environment first; these entries
	// override or extend it.
	Env []string `yaml:"env"`

	// --- network mode (URL must be set, Command must be empty) ---

	// URL is the HTTP(S) MCP endpoint of a remote network server.
	// Example: "http://remote-host:9000/mcp"
	URL string `yaml:"url"`

	// Headers contains HTTP headers sent on every request to the remote server.
	// Use this for authentication, e.g.:
	//   Authorization: "Bearer secret-token"
	Headers map[string]string `yaml:"headers"`

	// RetryInterval is how long to wait between reconnection attempts when the
	// remote server is unreachable. Accepts Go duration strings (e.g. "30s").
	// Defaults to "30s".
	RetryInterval string `yaml:"retry_interval"`

	// RequestTimeout is the per-request HTTP timeout when calling the remote
	// server. Accepts Go duration strings (e.g. "30s").
	// Defaults to "30s".
	RequestTimeout string `yaml:"request_timeout"`
}

// RetryIntervalDuration parses RetryInterval and returns the duration.
// Returns the default (30s) if the field is empty or invalid.
func (s *ServerConfig) RetryIntervalDuration() time.Duration {
	return parseDurationDefault(s.RetryInterval, 30*time.Second)
}

// RequestTimeoutDuration parses RequestTimeout and returns the duration.
// Returns the default (30s) if the field is empty or invalid.
func (s *ServerConfig) RequestTimeoutDuration() time.Duration {
	return parseDurationDefault(s.RequestTimeout, 30*time.Second)
}

func parseDurationDefault(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// Load reads and validates the YAML config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}

// applyDefaults fills in zero-value fields with their default values.
func (c *Config) applyDefaults() {
	if c.Server.DataDir == "" {
		c.Server.DataDir = "./.mcp-bridge"
	}
	if c.Server.Addr == "" {
		c.Server.Addr = ":7575"
	}
	if c.Server.Path == "" {
		c.Server.Path = "/mcp"
	}
	if c.Server.LogLevel == "" {
		c.Server.LogLevel = "info"
	}
}

func (c *Config) validate() error {
	switch strings.ToLower(c.Server.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("server.log_level %q is invalid; must be debug, info, warn, or error",
			c.Server.LogLevel)
	}

	tls := c.Server.TLS
	if tls.CertFile != "" && tls.KeyFile == "" {
		return fmt.Errorf("tls.key_file is required when tls.cert_file is set")
	}
	if tls.KeyFile != "" && tls.CertFile == "" {
		return fmt.Errorf("tls.cert_file is required when tls.key_file is set")
	}

	if len(c.Servers) == 0 {
		return fmt.Errorf("at least one server must be configured")
	}
	seen := make(map[string]struct{}, len(c.Servers))
	for i, s := range c.Servers {
		if s.Name == "" {
			return fmt.Errorf("server[%d]: name is required", i)
		}
		if strings.Contains(s.Name, "_") {
			return fmt.Errorf("server %q: name must not contain underscores", s.Name)
		}

		// Exactly one of command or url must be set.
		hasCommand := s.Command != ""
		hasURL := s.URL != ""
		if hasCommand && hasURL {
			return fmt.Errorf("server %q: command and url are mutually exclusive; set one or the other", s.Name)
		}
		if !hasCommand && !hasURL {
			return fmt.Errorf("server %q: one of command (stdio) or url (network) is required", s.Name)
		}

		// Validate duration fields for network servers.
		if hasURL {
			if s.RetryInterval != "" {
				if d, err := time.ParseDuration(s.RetryInterval); err != nil || d <= 0 {
					return fmt.Errorf("server %q: retry_interval %q is not a valid positive duration", s.Name, s.RetryInterval)
				}
			}
			if s.RequestTimeout != "" {
				if d, err := time.ParseDuration(s.RequestTimeout); err != nil || d <= 0 {
					return fmt.Errorf("server %q: request_timeout %q is not a valid positive duration", s.Name, s.RequestTimeout)
				}
			}
		}

		if _, dup := seen[s.Name]; dup {
			return fmt.Errorf("duplicate server name %q", s.Name)
		}
		seen[s.Name] = struct{}{}
	}
	return nil
}

// TLSEnabled reports whether any TLS mode is configured.
func (s *ServerSettings) TLSEnabled() bool {
	return s.TLS.CertFile != "" || s.TLS.AutoCert
}
