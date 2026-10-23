package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
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
	return applyMCPEnvironment(c), nil
}

func applyMCPEnvironment(c Config) Config {
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
	return c
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

// ReadPDFPasswords reads an owner-only password file, preserving spaces.
func ReadPDFPasswords(path string) ([]string, error) {
	file, err := openSecret(path)
	if err != nil {
		return nil, fmt.Errorf("open PDF passwords file: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, (16<<10)+1))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, fmt.Errorf("read PDF passwords file: %w", err)
	}
	defer clear(data)
	if len(data) > 16<<10 || !utf8.Valid(data) || strings.HasPrefix(string(data), "\ufeff") {
		return nil, errors.New("PDF passwords file must be valid UTF-8 without a BOM and at most 16 KiB")
	}
	passwords := []string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line != "" {
			passwords = append(passwords, line)
			if len(passwords) > 128 {
				return nil, errors.New("PDF passwords file lists more than 128 passwords")
			}
		}
	}
	return passwords, nil
}
