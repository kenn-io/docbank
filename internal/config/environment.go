package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const maxSecretBytes = 4096

// applyEnvironment overlays startup values without changing config.toml.
// Empty environment values preserve the file's setting.
func applyEnvironment(c Config) (Config, error) {
	if value := os.Getenv("DOCBANK_BIND_ADDR"); value != "" {
		c.Server.BindAddr = value
	}
	if value := os.Getenv("DOCBANK_API_PORT"); value != "" {
		port, err := strconv.Atoi(value)
		if err != nil || port < 0 || port > 65535 {
			return Config{}, errors.New("DOCBANK_API_PORT must be an integer from 0 through 65535")
		}
		c.Server.APIPort = port
	}
	if value := os.Getenv("DOCBANK_ALLOWED_HOSTS"); value != "" {
		c.Server.AllowedHosts = strings.Split(value, ",")
		for i := range c.Server.AllowedHosts {
			c.Server.AllowedHosts[i] = strings.TrimSpace(c.Server.AllowedHosts[i])
		}
	}
	key, selected, err := EnvironmentSecret("DOCBANK_API_KEY")
	if err != nil {
		return Config{}, err
	}
	if selected {
		c.Server.APIKey = key
	}
	if value := os.Getenv("DOCBANK_MCP_HTTP_ALLOWED_HOSTS"); value != "" {
		c.MCP.HTTP.AllowedHosts = strings.Split(value, ",")
		for i := range c.MCP.HTTP.AllowedHosts {
			c.MCP.HTTP.AllowedHosts[i] = strings.TrimSpace(c.MCP.HTTP.AllowedHosts[i])
		}
	}
	// The conventional inbound MCP secret selects a named process-local binding.
	// Its value is resolved only by the MCP process, never by daemon startup.
	if os.Getenv("DOCBANK_MCP_HTTP_TOKEN") != "" || os.Getenv("DOCBANK_MCP_HTTP_TOKEN_FILE") != "" {
		const prefix = "docbank-mcp-http-env"
		if c.CredentialBindings == nil {
			c.CredentialBindings = make(map[string]CredentialBindingConfig)
		}
		name := prefix
		for suffix := 1; ; suffix++ {
			if _, exists := c.CredentialBindings[name]; !exists {
				break
			}
			name = fmt.Sprintf("%s-%d", prefix, suffix)
		}
		c.MCP.HTTP.CredentialBinding = "credential:" + name
		c.CredentialBindings[name] = CredentialBindingConfig{EnvironmentVariable: "DOCBANK_MCP_HTTP_TOKEN"}
	}
	return c, nil
}

// EnvironmentSecret resolves NAME or NAME_FILE, rejecting conflicting nonempty
// sources and invalid files. Neither errors nor logs include the secret value.
func EnvironmentSecret(name string) (string, bool, error) {
	value, path := os.Getenv(name), os.Getenv(name+"_FILE")
	if value != "" && path != "" {
		return "", true, fmt.Errorf("%s and %s_FILE cannot both be nonempty", name, name)
	}
	if path != "" {
		file, err := openSecret(path)
		if err != nil {
			return "", true, fmt.Errorf("%s_FILE requires a readable, current-user-owned, non-symlink regular file with Unix mode 0400/0600 or a private protected Windows ACL: %w", name, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, maxSecretBytes+3))
		closeErr := file.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return "", true, fmt.Errorf("cannot read %s_FILE: %w", name, err)
		}
		value = string(data)
		if before, ok := strings.CutSuffix(value, "\r\n"); ok {
			value = before
		} else {
			value = strings.TrimSuffix(value, "\n")
		}
	}
	selected := value != "" || path != ""
	if selected && !validSecret(value) {
		return "", true, fmt.Errorf("%s must contain 1-4096 bytes without whitespace or control bytes", name)
	}
	return value, selected, nil
}

func validSecret(value string) bool {
	if value == "" || len(value) > maxSecretBytes {
		return false
	}
	for _, b := range []byte(value) {
		if b <= ' ' || b == 0x7f {
			return false
		}
	}
	return true
}
