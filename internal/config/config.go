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

// ServerConfig describes one MCP server — stdio, network, or local.
// Exactly one of Command, URL, or Local must be set.
type ServerConfig struct {
	// Name is used as the tool-name prefix (e.g. "git" → "git_status").
	// Must be unique across all servers.
	Name string `yaml:"name"`

	// --- stdio mode (Command must be set, URL and Local must be empty) ---

	// Command is the absolute (or PATH-resolvable) path to the binary.
	Command string `yaml:"command"`

	// Args are optional command-line arguments forwarded to the binary.
	Args []string `yaml:"args"`

	// Env contains optional KEY=VALUE pairs injected into the child environment.
	// The child always inherits the bridge's environment first; these entries
	// override or extend it.
	Env []string `yaml:"env"`

	// --- network mode (URL must be set, Command and Local must be empty) ---

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

	// Insecure disables TLS certificate verification when connecting to the
	// remote server. Enable this only when the remote uses a self-signed or
	// otherwise untrusted certificate and you accept the security implications.
	// Defaults to false.
	Insecure bool `yaml:"insecure"`

	// --- local mode (Local must be non-empty, Command and URL must be empty) ---

	// Timeout is the default execution timeout for all local tools in this
	// server block. Accepts Go duration strings (e.g. "30s").
	// Defaults to "30s". Individual tools may override this with their own
	// timeout field.
	Timeout string `yaml:"timeout"`

	// Local lists the tools exposed by this local server. Each tool is either
	// an exec command (set command) or an HTTP request (set url).
	// Exec tools may also receive runtime arguments via tools/call.arguments.args.
	Local []LocalTool `yaml:"local"`
}

// CommandTokens holds a command as an ordered list of tokens.
// In YAML it accepts either a scalar string (split on whitespace) or a
// sequence of strings.
//
//	Scalar: "ls -alh {{path}}"          → tokens ["ls", "-alh", "{{path}}"]
//	List:   ["sh", "-c", "cmd | wc -l"] → tokens as given
//
// Use scalar form for simple commands. Use list form when a token contains
// spaces or you need an explicit shell pipeline (["sh", "-c", "..."]).
// Metacharacter detection (|, &, ;, >, etc.) applies only to scalar form.
type CommandTokens struct {
	Tokens []string // split tokens
	Raw    string   // original scalar string; empty when parsed from a YAML sequence
}

// IsEmpty reports whether the command has no tokens.
func (c CommandTokens) IsEmpty() bool { return len(c.Tokens) == 0 }

// UnmarshalYAML implements yaml.Unmarshaler.
// Accepts a YAML scalar (split on whitespace) or a YAML sequence of strings.
func (c *CommandTokens) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		c.Raw = value.Value
		c.Tokens = strings.Fields(value.Value)
		return nil
	case yaml.SequenceNode:
		c.Raw = ""
		c.Tokens = make([]string, 0, len(value.Content))
		for i, n := range value.Content {
			if n.Kind != yaml.ScalarNode {
				return fmt.Errorf("command[%d]: sequence elements must be strings", i)
			}
			c.Tokens = append(c.Tokens, n.Value)
		}
		return nil
	default:
		return fmt.Errorf("command: must be a string or a list of strings")
	}
}

// LocalParam defines a named parameter for a local tool.
// When a tool has Params, tools/call arguments are matched by name and
// substituted into {{name}} placeholders in command, args, url, headers,
// and body. When Params is empty, exec tools fall back to the generic
// {"args": [...]} positional interface.
type LocalParam struct {
	// Name is the parameter name, referenced as {{name}} in templates.
	Name string `yaml:"name"`

	// Description is shown to MCP clients in the tool's input schema.
	Description string `yaml:"description"`

	// Type is the JSON Schema type: string (default), array, integer, number, boolean.
	Type string `yaml:"type"`

	// Required marks this parameter as required in the input schema.
	Required bool `yaml:"required"`
}

// LocalTool defines a single tool in a local server block.
// Exactly one of Command or URL must be set.
type LocalTool struct {
	// Tool is the tool name exposed to MCP clients (prefixed with the server
	// name, e.g. "sysadmin_list_tmp_files").
	// Must be unique within the server.
	Tool string `yaml:"tool"`

	// Description is a human-readable description shown to MCP clients.
	Description string `yaml:"description"`

	// Timeout overrides the server-level default timeout for this tool.
	// Accepts Go duration strings (e.g. "10s"). Defaults to the server-level
	// timeout (which itself defaults to "30s").
	Timeout string `yaml:"timeout"`

	// Params defines named parameters for {{name}} template substitution.
	// When set, tools/call arguments are matched by name and substituted into
	// placeholders in command, args, url, headers, and body.
	// When empty, exec tools accept {"args": [...]}; HTTP tools accept no arguments.
	Params []LocalParam `yaml:"params"`

	// --- exec mode (Command must be set, URL must be empty) ---

	// Command is the command to execute as a scalar string or a list of strings.
	// Scalar: "ls -alh {{path}}"          — split on whitespace.
	// List:   ["sh", "-c", "cmd | wc -l"] — tokens as given; use when a token
	//         contains spaces or you need an explicit shell pipeline.
	// Supports {{name}} placeholders when Params is set.
	// Shell metacharacters (|, &, ;, >, etc.) in scalar form cause the command
	// to run via sh -c automatically.
	Command CommandTokens `yaml:"command"`

	// --- http mode (URL must be set, Command must be empty) ---

	// URL is the HTTP endpoint to call. Supports {{name}} placeholders when Params is set.
	URL string `yaml:"url"`

	// Method is the HTTP method (GET, POST, PUT, DELETE, etc.).
	// Defaults to "GET".
	Method string `yaml:"method"`

	// Headers contains HTTP headers sent on every request.
	// Values support {{name}} placeholders when Params is set.
	Headers map[string]string `yaml:"headers"`

	// Body is an optional request body (used with POST/PUT).
	// Supports {{name}} placeholders when Params is set.
	Body string `yaml:"body"`
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

// TimeoutDuration parses Timeout and returns the duration.
// Returns the default (30s) if the field is empty or invalid.
// Used as the default timeout for all local tools in this server block.
func (s *ServerConfig) TimeoutDuration() time.Duration {
	return parseDurationDefault(s.Timeout, 30*time.Second)
}

// ToolTimeoutDuration returns the effective timeout for a local tool:
// the tool's own Timeout if set, otherwise the server-level default.
func (s *ServerConfig) ToolTimeoutDuration(t *LocalTool) time.Duration {
	if t.Timeout != "" {
		if d, err := time.ParseDuration(t.Timeout); err == nil && d > 0 {
			return d
		}
	}
	return s.TimeoutDuration()
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

		hasCommand := s.Command != ""
		hasURL := s.URL != ""
		hasLocal := len(s.Local) > 0

		// Mutual exclusion across all three modes.
		if hasCommand && hasURL {
			return fmt.Errorf("server %q: command and url are mutually exclusive; set exactly one of command, url, or local", s.Name)
		}
		if hasCommand && hasLocal {
			return fmt.Errorf("server %q: command and local are mutually exclusive; set exactly one of command, url, or local", s.Name)
		}
		if hasURL && hasLocal {
			return fmt.Errorf("server %q: url and local are mutually exclusive; set exactly one of command, url, or local", s.Name)
		}
		if !hasCommand && !hasURL && !hasLocal {
			return fmt.Errorf("server %q: one of command (stdio), url (network), or local is required", s.Name)
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

		// Validate local tools.
		if hasLocal {
			if s.Timeout != "" {
				if d, err := time.ParseDuration(s.Timeout); err != nil || d <= 0 {
					return fmt.Errorf("server %q: timeout %q is not a valid positive duration", s.Name, s.Timeout)
				}
			}
			seenTools := make(map[string]struct{}, len(s.Local))
			for j, t := range s.Local {
				if t.Tool == "" {
					return fmt.Errorf("server %q: local[%d]: tool name is required", s.Name, j)
				}
				if _, dup := seenTools[t.Tool]; dup {
					return fmt.Errorf("server %q: duplicate local tool name %q", s.Name, t.Tool)
				}
				seenTools[t.Tool] = struct{}{}

				hasToolCmd := !t.Command.IsEmpty()
				hasToolURL := t.URL != ""
				if hasToolCmd && hasToolURL {
					return fmt.Errorf("server %q: local tool %q: command and url are mutually exclusive; set exactly one", s.Name, t.Tool)
				}
				if !hasToolCmd && !hasToolURL {
					return fmt.Errorf("server %q: local tool %q: one of command (exec) or url (http) is required", s.Name, t.Tool)
				}

				if t.Timeout != "" {
					if d, err := time.ParseDuration(t.Timeout); err != nil || d <= 0 {
						return fmt.Errorf("server %q: local tool %q: timeout %q is not a valid positive duration", s.Name, t.Tool, t.Timeout)
					}
				}

				// Validate named params.
				validParamTypes := map[string]bool{
					"string": true, "array": true, "integer": true,
					"number": true, "boolean": true,
				}
				seenParams := make(map[string]struct{}, len(t.Params))
				for k, p := range t.Params {
					if p.Name == "" {
						return fmt.Errorf("server %q: local tool %q: params[%d]: name is required", s.Name, t.Tool, k)
					}
					if _, dup := seenParams[p.Name]; dup {
						return fmt.Errorf("server %q: local tool %q: duplicate param name %q", s.Name, t.Tool, p.Name)
					}
					seenParams[p.Name] = struct{}{}
					if p.Type != "" && !validParamTypes[p.Type] {
						return fmt.Errorf("server %q: local tool %q: param %q: type %q is invalid; must be string, array, integer, number, or boolean",
							s.Name, t.Tool, p.Name, p.Type)
					}
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
