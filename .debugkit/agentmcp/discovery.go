// Package agentmcp discovers published MCP listeners and registers servers in
// agent configuration files. Callers choose executables, server names, and
// target agents; this package does not maintain an application catalog.
package agentmcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Listener is one entry in a CLI's `mcp status --json` response. BackendURL
// identifies the backend served by this listener, not the MCP endpoint itself.
type Listener struct {
	PID        int    `json:"pid"`
	Transport  string `json:"transport"`
	URL        string `json:"url"`
	BackendURL string `json:"backend_url"`
	TokenPath  string `json:"token_path"`
}

// Discover runs executable with `mcp status --json` and returns all advertised
// HTTP listeners. It does not start servers or fall back to a stdio command.
// The caller should supply a context deadline and query only trusted CLIs.
func Discover(ctx context.Context, executable string) ([]Listener, error) {
	data, err := exec.CommandContext(ctx, executable, "mcp", "status", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("discover MCP listeners with %s: %w", executable, errors.Join(err, ctx.Err()))
	}
	var listeners []Listener
	if err := json.Unmarshal(data, &listeners); err != nil {
		return nil, fmt.Errorf("decode MCP listeners from %s: %w", executable, err)
	}
	if strings.TrimSpace(string(data)) == "null" {
		return nil, errors.New("MCP status must return a listener array")
	}
	for _, listener := range listeners {
		if listener.Transport != "http" {
			return nil, fmt.Errorf("unsupported MCP listener transport %q", listener.Transport)
		}
		if err := validateURL(listener.URL); err != nil {
			return nil, fmt.Errorf("invalid MCP listener: %w", err)
		}
	}
	return listeners, nil
}

// Server reads this listener's optional bearer token and describes its HTTP
// registration. TokenPath is trusted local input from the queried CLI. Tokens
// are copied into Headers; future token rotation requires registration again.
func (listener Listener) Server() (Server, error) {
	if listener.Transport != "http" {
		return Server{}, fmt.Errorf("unsupported MCP listener transport %q", listener.Transport)
	}
	if err := validateURL(listener.URL); err != nil {
		return Server{}, err
	}
	server := Server{URL: listener.URL}
	if listener.TokenPath != "" {
		data, err := os.ReadFile(listener.TokenPath)
		if err != nil {
			return Server{}, fmt.Errorf("read MCP listener token: %w", err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return Server{}, errors.New("MCP listener token is empty")
		}
		server.Headers = map[string]string{"Authorization": "Bearer " + token}
	}
	return server, nil
}
