// Package config loads the optional $DOCBANK_HOME/config.toml. Every value
// has a default; the file's absence is not an error. There are no per-field
// env or flag overrides — the only environment knob is DOCBANK_HOME.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
	kitdaemon "go.kenn.io/kit/daemon"
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

// Config is the full contents of config.toml.
type Config struct {
	Server ServerConfig `toml:"server"`
	Web    WebConfig    `toml:"web"`
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
	return c, nil
}

// Validate enforces the bind/key policy: an unspecified address (0.0.0.0,
// ::) is always rejected because it binds every interface including public
// ones; an unset api_key is valid only on loopback, where the daemon
// generates and self-publishes an ephemeral key rather than serving
// unauthenticated (see cmd/docbank/serve.go); and any other
// non-loopback address must be non-public (kit RequireNonPublic) and carry
// a configured api_key, since remote clients can't read the runtime record
// an ephemeral key would be published through.
func (c Config) Validate() error {
	host := c.Server.BindAddr
	if isLoopbackHost(host) {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("[server] bind_addr %q: not an IP address or localhost", host)
	}
	if ip.IsUnspecified() {
		return fmt.Errorf(
			"[server] bind_addr %q: unspecified address binds every interface; "+
				"use a concrete address", host)
	}
	if c.Server.APIKey == "" {
		return fmt.Errorf("[server] bind_addr %q requires a non-empty api_key "+
			"(keyless mode is loopback-only)", host)
	}
	if err := kitdaemon.RequireNonPublic(net.JoinHostPort(host, "0")); err != nil {
		return fmt.Errorf("[server] bind_addr %q: %w", host, err)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
