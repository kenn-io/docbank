// Package config loads the optional $DOCBANK_HOME/config.toml. Every value
// has a default; the file's absence is not an error. There are no per-field
// env or flag overrides — the only environment knob is DOCBANK_HOME.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Duration is a time.Duration that unmarshals from a TOML string such as
// "30m"; "0" disables the associated timeout.
//
//nolint:recvcheck // UnmarshalText needs a pointer receiver; Std is value semantics by design.
type Duration time.Duration

// UnmarshalText parses a duration string, rejecting negative durations.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", string(b), err)
	}
	if v < 0 {
		return fmt.Errorf("invalid duration %q: must not be negative", string(b))
	}
	*d = Duration(v)
	return nil
}

// Std returns d as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// ServerConfig configures the docbank API daemon's listen address and idle
// shutdown behavior.
type ServerConfig struct {
	BindAddr    string   `toml:"bind_addr"`    // default "127.0.0.1"
	APIPort     int      `toml:"api_port"`     // default 0 (ephemeral)
	APIKey      string   `toml:"api_key"`      // default "" (ephemeral per-run key on loopback)
	IdleTimeout Duration `toml:"idle_timeout"` // default 30m; background daemons only
}

// WebConfig controls the built-in web UI.
type WebConfig struct {
	Enabled bool `toml:"enabled"` // default true
}

// BackupConfig configures the default immutable snapshot repository and its
// compression policy. An empty Repo keeps backup commands available through
// an explicit request path without silently choosing storage under the vault.
type BackupConfig struct {
	Repo      string `toml:"repo"`
	ZstdLevel int    `toml:"zstd_level"`
}

// Config is the full contents of config.toml.
type Config struct {
	Server ServerConfig `toml:"server"`
	Web    WebConfig    `toml:"web"`
	Backup BackupConfig `toml:"backup"`
}

// Default returns the configuration used when config.toml is absent.
func Default() Config {
	return Config{
		Server: ServerConfig{
			BindAddr:    "127.0.0.1",
			IdleTimeout: Duration(30 * time.Minute),
		},
		Web: WebConfig{Enabled: true},
	}
}

// Load reads <root>/config.toml, returning Default() if the file does not
// exist. An unrecognized key is treated as a typo and rejected.
func Load(root string) (Config, error) {
	c := Default()
	path := filepath.Join(root, "config.toml")
	md, err := toml.DecodeFile(path, &c)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("loading %s: %w", path, err)
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		return Config{}, fmt.Errorf("loading %s: unknown key %q (typo?)", path, undec[0].String())
	}
	if err := resolveBackupRepo(root, &c.Backup); err != nil {
		return Config{}, fmt.Errorf("loading %s: %w", path, err)
	}
	return c, nil
}

func resolveBackupRepo(root string, backup *BackupConfig) error {
	if backup.Repo == "" {
		return nil
	}
	repo := backup.Repo
	if repo == "~" || strings.HasPrefix(repo, "~/") || strings.HasPrefix(repo, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolving [backup] repo %q: %w", repo, err)
		}
		if repo == "~" {
			repo = home
		} else {
			repo = filepath.Join(home, strings.TrimLeft(repo[1:], `/\`))
		}
	}
	if !filepath.IsAbs(repo) {
		repo = filepath.Join(root, repo)
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return fmt.Errorf("resolving [backup] repo %q: %w", backup.Repo, err)
	}
	backup.Repo = filepath.Clean(abs)
	return nil
}

// Validate enforces the bind policy: loopback only. The API is plain HTTP,
// so any non-loopback bind — even a keyed, private-network one — would put
// the API key and vault contents on the wire in cleartext. Remote access
// goes through an SSH tunnel or VPN to the loopback listener until the
// daemon grows TLS. An unset api_key stays valid: the daemon generates and
// self-publishes an ephemeral key rather than serving unauthenticated (see
// cmd/docbank/daemon.go).
func (c Config) Validate() error {
	if c.Backup.ZstdLevel != 0 && (c.Backup.ZstdLevel < 1 || c.Backup.ZstdLevel > 19) {
		return fmt.Errorf("[backup] zstd_level %d: want 0 or 1-19", c.Backup.ZstdLevel)
	}
	host := c.Server.BindAddr
	if isLoopbackHost(host) {
		return nil
	}
	if net.ParseIP(host) == nil {
		return fmt.Errorf("[server] bind_addr %q: not an IP address or localhost", host)
	}
	return fmt.Errorf("[server] bind_addr %q: the API is plain HTTP, so binds are "+
		"loopback-only; reach a remote docbank through an SSH tunnel or VPN", host)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
