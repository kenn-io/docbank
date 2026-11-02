package agentmcp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/atomicfile"
)

// InstallOptions registers one caller-owned server name. An existing entry
// with that exact name is replaced, including any transport-specific options.
// ConfigPath overrides the user config path with a file using the same top-level
// schema. Nested Claude local-scope entries and Copilot bare server maps are not
// handled by this option. Use a stable, application-namespaced Name.
type InstallOptions struct {
	ConfigPath string
	Name       string
	Server     Server
}

// Result contains the planned or completed config mutation. Data can include
// credentials and must be handled like the original config file.
type Result struct {
	Agent      Agent
	ConfigPath string
	Changed    bool
	Data       []byte
}

// MarshalConfig returns a complete standalone client configuration document
// containing one named server for the caller to save or merge. It reads and
// writes no client files. Data can include credentials.
func MarshalConfig(agent Agent, name string, server Server) ([]byte, error) {
	entry, err := prepareConfig(agent, name, server)
	if err != nil {
		return nil, err
	}
	data, _, err := planConfig(agent, nil, name, entry)
	return data, err
}

func prepareConfig(agent Agent, name string, server Server) (map[string]any, error) {
	if _, ok := LookupProfile(agent); !ok {
		return nil, fmt.Errorf("unsupported MCP agent %q", agent)
	}
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("MCP server name is required")
	}
	return server.native(agent)
}

func planConfig(agent Agent, original []byte, name string, entry map[string]any) ([]byte, bool, error) {
	if agent == AgentCodex || agent == AgentHermes {
		return planDocument(agent, original, name, entry)
	}
	return planJSON(agent, original, name, entry)
}

// PlanInstall computes a registration without writing files. Other server
// entries, settings, and comments are retained. Comments on replaced fields
// remain next to the replacement. Formatting may change; unchanged files retain
// all bytes.
func PlanInstall(agent Agent, opts InstallOptions) (Result, error) {
	entry, err := prepareConfig(agent, opts.Name, opts.Server)
	if err != nil {
		return Result{}, err
	}
	path := opts.ConfigPath
	if path == "" {
		path, err = ConfigPath(agent)
		if err != nil {
			return Result{}, err
		}
	}
	original, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("read MCP config %s: %w", path, err)
	}
	data, changed, err := planConfig(agent, original, opts.Name, entry)
	if err != nil {
		return Result{}, fmt.Errorf("plan MCP config %s: %w", path, err)
	}
	return Result{Agent: agent, ConfigPath: path, Changed: changed, Data: data}, nil
}

// Install registers a named server atomically. Callers must serialize changes
// to the same config file. It preserves existing modes and follows an existing
// config symlink. An error wrapping atomicfile.ErrPublished returns the populated
// result because the mutation has already become visible.
func Install(agent Agent, opts InstallOptions) (Result, error) {
	result, err := PlanInstall(agent, opts)
	if err != nil || !result.Changed {
		return result, err
	}
	writePath := result.ConfigPath
	options := []atomicfile.Option{atomicfile.WithPerm(0o600), atomicfile.WithPreserveMode()}
	info, err := os.Lstat(writePath)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		options = append(options, atomicfile.WithFollowLink())
		writePath, err = filepath.EvalSymlinks(writePath)
		if err != nil {
			return Result{}, fmt.Errorf("resolve MCP config link: %w", err)
		}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return Result{}, fmt.Errorf("inspect MCP config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(writePath), 0o700); err != nil {
		return Result{}, fmt.Errorf("create MCP config directory: %w", err)
	}
	if err := atomicfile.WriteFile(result.ConfigPath, result.Data, options...); err != nil {
		if errors.Is(err, atomicfile.ErrPublished) {
			return result, fmt.Errorf("write MCP config: %w", err)
		}
		return Result{}, fmt.Errorf("write MCP config: %w", err)
	}
	return result, nil
}
