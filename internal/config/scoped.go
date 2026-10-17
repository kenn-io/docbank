package config

import (
	"errors"
	"fmt"
	"io"
	"net"

	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/internal/httpboundary"
)

// CheckClient rejects global parse errors and warns about unused file policy.
// Clients use the running daemon's configuration and runtime credentials, not
// startup environment overrides from the client shell.
func CheckClient(root string, diagnostics io.Writer) error {
	c, err := Parse(root)
	if err != nil {
		return err
	}
	c, err = resolveTOML(root, c)
	if err == nil {
		err = c.Validate()
	}
	if err != nil {
		_, _ = fmt.Fprintf(diagnostics, "warning: file-only configuration check: %v; the running daemon keeps its startup settings and may supply environment overrides\n", err)
	}

	return nil
}

// LoadWeb reads only the options used by the web launcher. Daemon startup
// separately checks the public-origin/API-key relationship in its environment.
func LoadWeb(root string) (Config, error) {
	c, err := Parse(root)
	if err != nil {
		return Config{}, err
	}
	if err := c.ValidateWeb(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// LoadMCPHTTP reads the independent MCP HTTP policy and its selected binding.
// Daemon-only paths, processing policy, and environment overrides are unused.
func LoadMCPHTTP(root string) (Config, error) {
	c, err := Parse(root)
	if err != nil {
		return Config{}, err
	}
	c = applyMCPEnvironment(c)
	if err := c.ValidateMCPHTTP(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// ValidateWeb checks web options without requiring the daemon's startup key.
func (c Config) ValidateWeb() error {
	if err := validateWebOptions(c.Web); err != nil {
		return err
	}
	return c.Web.MapTiles.Validate()
}

// ValidateMedia checks the subsystem configuration.
func (c Config) ValidateMedia() error {
	if c.Media.MaxSourceBytes < 0 || c.Media.MaxSourceBytes > media.MaxMediaSourceBytes {
		return fmt.Errorf("[media] max_source_bytes must be between 1 and %d (or 0 for default)", media.MaxMediaSourceBytes)
	}
	return nil
}

// validateDaemonWeb checks the subsystem configuration.
func (c Config) validateDaemonWeb() error {
	if err := validateWebConfig(c); err != nil {
		return err
	}
	if err := c.Web.MapTiles.Validate(); err != nil {
		return err
	}
	return nil
}

// ValidatePageRuntime checks the subsystem configuration.
func (c Config) ValidatePageRuntime() error {
	if c.PageRuntime != nil {
		if err := c.PageRuntime.Validate(); err != nil {
			return fmt.Errorf("[page_runtime]: %w", err)
		}
	}
	return nil
}

// ValidateBackup checks the subsystem configuration.
func (c Config) ValidateBackup() error {
	if c.Backup.ZstdLevel != 0 && (c.Backup.ZstdLevel < 1 || c.Backup.ZstdLevel > 19) {
		return fmt.Errorf("[backup] zstd_level %d: want 0 or 1-19", c.Backup.ZstdLevel)
	}
	return nil
}

// ValidateStorage checks the subsystem configuration.
func (c Config) ValidateStorage() error {
	if c.Storage.PackMaxBytes < 0 {
		return errors.New("[storage] pack_max_bytes must not be negative")
	}
	if c.Storage.PackInterval.Std() < 0 {
		return errors.New("[storage] pack_interval must not be negative")
	}
	if c.Storage.PackInterval.Std() > 0 && c.Storage.PackMaxBytes == 0 {
		return errors.New("[storage] pack_max_bytes must be positive when pack_interval is enabled")
	}
	return nil
}

// ValidateStoreBindings checks the subsystem configuration.
func (c Config) ValidateStoreBindings() error {
	return validateStoreBindings(c.StoreBindings)
}

// ValidateProcessing checks the subsystem configuration.
func (c Config) ValidateProcessing() error {
	return validateProcessingProfiles(c)
}

// ValidateMCPHTTP checks the subsystem configuration.
func (c Config) ValidateMCPHTTP() error {
	return validateMCPConfig(c)
}

// ValidateMediaOrigins checks the subsystem configuration.
func (c Config) ValidateMediaOrigins() error {
	return validateMediaOrigins(c)
}

// ValidateWatches checks the subsystem configuration.
func (c Config) ValidateWatches() error {
	for _, watch := range c.Watches {
		if err := validateWatch(watch); err != nil {
			return err
		}
	}
	return nil
}

// ValidateServer checks the subsystem configuration.
func (c Config) ValidateServer() error {
	if c.Server.IdleTimeout.Std() < 0 {
		return errors.New("[server] idle_timeout must not be negative")
	}
	if c.Server.APIPort < 0 || c.Server.APIPort > 65535 {
		return errors.New("[server] api_port must be from 0 through 65535")
	}
	if c.Server.APIKey != "" && !validSecret(c.Server.APIKey) {
		return errors.New("[server] api_key must contain 1-4096 bytes without whitespace or control bytes")
	}
	if err := httpboundary.ValidateHosts(c.Server.AllowedHosts); err != nil {
		return fmt.Errorf("[server] allowed_hosts: %w", err)
	}
	host := c.Server.BindAddr
	if isLoopbackHost(host) {
		return nil
	}
	if net.ParseIP(host) == nil {
		return fmt.Errorf("[server] bind_addr %q: not an IP address or localhost", host)
	}
	if c.Server.APIKey == "" {
		return errors.New("[server] non-loopback bind_addr requires an explicit api_key; plain HTTP requires a trusted network or encrypted tunnel")
	}
	return nil
}
