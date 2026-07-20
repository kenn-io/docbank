// Package config loads the optional $DOCBANK_HOME/config.toml. Every value
// has a default; the file's absence is not an error. There are no per-field
// env or flag overrides — the only environment knob is DOCBANK_HOME.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
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

// EmbeddingsConfig configures the optional OpenAI-compatible encoder used to
// build Docbank's local, derived semantic-search index. An empty BaseURL and
// Model leave embeddings disabled and ordinary lexical search unchanged.
type EmbeddingsConfig struct {
	BaseURL         string   `toml:"base_url"`
	Model           string   `toml:"model"`
	APIKey          string   `toml:"api_key"`
	APIKeyEnv       string   `toml:"api_key_env"`
	FingerprintSalt string   `toml:"fingerprint_salt"`
	Dimensions      int      `toml:"dimensions"`
	BatchSize       int      `toml:"batch_size"`
	Concurrency     int      `toml:"concurrency"`
	Timeout         Duration `toml:"timeout"`
}

// Enabled reports whether an embeddings endpoint is configured. Validate
// rejects partial configuration, so callers never observe a half-enabled
// section after successful config loading.
func (e EmbeddingsConfig) Enabled() bool {
	return strings.TrimSpace(e.BaseURL) != "" && strings.TrimSpace(e.Model) != ""
}

// ResolvedAPIKey returns the inline key or the value of APIKeyEnv. Environment
// indirection keeps bearer credentials out of config.toml when desired.
func (e EmbeddingsConfig) ResolvedAPIKey() string {
	if e.APIKey != "" {
		return e.APIKey
	}
	if e.APIKeyEnv != "" {
		return os.Getenv(strings.TrimSpace(e.APIKeyEnv))
	}
	return ""
}

// WatchConfig describes one daemon-owned local inbox. Name and each relative
// source path form the stable, portable source identity; Source itself is a
// machine-local location and is intentionally not archive metadata.
type WatchConfig struct {
	Name         string   `toml:"name"`
	Source       string   `toml:"source"`
	Destination  string   `toml:"destination"`
	SettleTime   Duration `toml:"settle_time"`
	ScanInterval Duration `toml:"scan_interval"`
	Exclude      []string `toml:"exclude"`
}

// Config is the full contents of config.toml.
type Config struct {
	Server     ServerConfig     `toml:"server"`
	Web        WebConfig        `toml:"web"`
	Backup     BackupConfig     `toml:"backup"`
	Embeddings EmbeddingsConfig `toml:"embeddings"`
	Watches    []WatchConfig    `toml:"watch"`
}

// Default returns the configuration used when config.toml is absent.
func Default() Config {
	return Config{
		Server: ServerConfig{
			BindAddr:    "127.0.0.1",
			IdleTimeout: Duration(30 * time.Minute),
		},
		Web: WebConfig{Enabled: true},
		Embeddings: EmbeddingsConfig{
			Dimensions: 768, BatchSize: 32, Concurrency: 2,
			Timeout: Duration(30 * time.Second),
		},
	}
}

// Load reads <root>/config.toml, returning Default() if the file does not
// exist. An unrecognized key is treated as a typo and rejected.
func Load(root string) (Config, error) {
	c := Default()
	path := filepath.Join(root, "config.toml")
	file, err := openConfig(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("loading %s: %w", path, err)
	}
	md, decodeErr := toml.NewDecoder(file).Decode(&c)
	closeErr := file.Close()
	if decodeErr != nil {
		return Config{}, fmt.Errorf("loading %s: %w", path, decodeErr)
	}
	if closeErr != nil {
		return Config{}, fmt.Errorf("loading %s: %w", path, closeErr)
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		return Config{}, fmt.Errorf("loading %s: unknown key %q (typo?)", path, undec[0].String())
	}
	if err := resolveBackupRepo(root, &c.Backup); err != nil {
		return Config{}, fmt.Errorf("loading %s: %w", path, err)
	}
	if err := resolveWatches(&c); err != nil {
		return Config{}, fmt.Errorf("loading %s: %w", path, err)
	}
	return c, nil
}

const (
	defaultWatchSettleTime   = 30 * time.Second
	defaultWatchScanInterval = 5 * time.Second
)

func resolveWatches(c *Config) error {
	home, homeErr := os.UserHomeDir()
	names := make(map[string]struct{}, len(c.Watches))
	sources := make(map[string]string, len(c.Watches))
	for i := range c.Watches {
		watch := &c.Watches[i]
		if _, exists := names[watch.Name]; exists {
			return fmt.Errorf("[[watch]] name %q is duplicated", watch.Name)
		}
		names[watch.Name] = struct{}{}
		if watch.Source == "~" || strings.HasPrefix(watch.Source, "~/") ||
			strings.HasPrefix(watch.Source, `~\`) {
			if homeErr != nil {
				return fmt.Errorf("resolving [[watch]] %q source %q: %w",
					watch.Name, watch.Source, homeErr)
			}
			if watch.Source == "~" {
				watch.Source = home
			} else {
				watch.Source = filepath.Join(home, strings.TrimLeft(watch.Source[1:], `/\`))
			}
		}
		if watch.Source != "" && !filepath.IsAbs(watch.Source) {
			return fmt.Errorf("[[watch]] %q source %q must be absolute or start with ~/",
				watch.Name, watch.Source)
		}
		if watch.Source != "" {
			abs, err := filepath.Abs(watch.Source)
			if err != nil {
				return fmt.Errorf("resolving [[watch]] %q source %q: %w",
					watch.Name, watch.Source, err)
			}
			watch.Source = filepath.Clean(abs)
			if prior, exists := sources[watch.Source]; exists {
				return fmt.Errorf("[[watch]] %q and %q use the same source %q",
					prior, watch.Name, watch.Source)
			}
			sources[watch.Source] = watch.Name
		}
		if watch.SettleTime.Std() == 0 {
			watch.SettleTime = Duration(defaultWatchSettleTime)
		}
		if watch.ScanInterval.Std() == 0 {
			watch.ScanInterval = Duration(defaultWatchScanInterval)
		}
	}
	return nil
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
	for _, watch := range c.Watches {
		if err := validateWatch(watch); err != nil {
			return err
		}
	}
	if err := validateEmbeddings(c.Embeddings); err != nil {
		return err
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

func validateEmbeddings(e EmbeddingsConfig) error {
	hasBase := strings.TrimSpace(e.BaseURL) != ""
	hasModel := strings.TrimSpace(e.Model) != ""
	if hasBase != hasModel {
		return errors.New("[embeddings] base_url and model must both be set or both omitted")
	}
	if e.APIKey != "" && strings.TrimSpace(e.APIKeyEnv) != "" {
		return errors.New("[embeddings] api_key and api_key_env are mutually exclusive")
	}
	if !hasBase {
		if e.APIKey != "" || strings.TrimSpace(e.APIKeyEnv) != "" ||
			strings.TrimSpace(e.FingerprintSalt) != "" {
			return errors.New("[embeddings] credentials and fingerprint_salt require base_url and model")
		}
		return nil
	}
	if e.Dimensions < 1 || e.Dimensions > 8192 {
		return errors.New("[embeddings] dimensions must be between 1 and 8192")
	}
	if e.BatchSize < 1 || e.BatchSize > 1000 {
		return errors.New("[embeddings] batch_size must be between 1 and 1000")
	}
	if e.Concurrency < 1 || e.Concurrency > 64 {
		return errors.New("[embeddings] concurrency must be between 1 and 64")
	}
	if e.Timeout.Std() <= 0 {
		return errors.New("[embeddings] timeout must be positive")
	}
	target, err := url.Parse(e.BaseURL)
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		return fmt.Errorf("[embeddings] base_url %q must be an absolute HTTP(S) URL", e.BaseURL)
	}
	if target.User != nil || target.RawQuery != "" || target.Fragment != "" {
		return errors.New("[embeddings] base_url must not contain credentials, a query, or a fragment")
	}
	if target.Scheme == "http" && !isLoopbackHost(target.Hostname()) {
		return errors.New("[embeddings] plaintext HTTP is allowed only for a loopback endpoint")
	}
	return nil
}

func validateWatch(watch WatchConfig) error {
	if watch.Name == "" || len(watch.Name) > 64 {
		return errors.New("[[watch]] name must contain 1-64 characters")
	}
	for _, char := range watch.Name {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') ||
			strings.ContainsRune("-_.", char) {
			continue
		}
		return fmt.Errorf("[[watch]] name %q contains unsupported characters", watch.Name)
	}
	if watch.Source == "" {
		return fmt.Errorf("[[watch]] %q source is required", watch.Name)
	}
	if !filepath.IsAbs(watch.Source) {
		return fmt.Errorf("[[watch]] %q source %q is not absolute", watch.Name, watch.Source)
	}
	if !strings.HasPrefix(watch.Destination, "/") {
		return fmt.Errorf("[[watch]] %q destination %q must be an absolute virtual path",
			watch.Name, watch.Destination)
	}
	if watch.SettleTime.Std() <= 0 {
		return fmt.Errorf("[[watch]] %q settle_time must be positive", watch.Name)
	}
	if watch.ScanInterval.Std() <= 0 {
		return fmt.Errorf("[[watch]] %q scan_interval must be positive", watch.Name)
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
