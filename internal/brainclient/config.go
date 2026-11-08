// Package brainclient stores client publication configuration, separate from
// daemon-owned source and content authority.
package brainclient

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

// ConnectionConfig pins one named publisher's local source and daemon target.
// KeyFile is a reference to an existing key source, never the key value.
// Durations are stored as integer nanoseconds. Defaults are selected by the
// caller; minimum age may be zero, while settle and scan intervals must be positive.
type ConnectionConfig struct {
	Name           string        `json:"name"`
	SourceRoot     string        `json:"source_root"`
	Destination    string        `json:"destination"`
	LocalVaultRoot string        `json:"local_vault_root"`
	RemoteOrigin   string        `json:"remote_origin"`
	KeyFile        string        `json:"key_file"`
	Exclude        []string      `json:"exclude"`
	SettleTime     time.Duration `json:"settle_time"`
	MinimumAge     time.Duration `json:"minimum_age"`
	ScanInterval   time.Duration `json:"scan_interval"`
	IndexLocal     bool          `json:"index_local"`
}

// DefaultConfigRoot returns the platform's private client registry location.
// It creates no directories and does not resolve a daemon vault.
func DefaultConfigRoot() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolving client configuration directory: %w", err)
	}
	return filepath.Abs(filepath.Join(base, "docbank", "brain"))
}

// ReadKey preserves the push command's key-source semantics. An explicit file
// always wins; unreadable/invalid files never fall back to the environment.
func ReadKey(keyFile string) (string, error) {
	key := os.Getenv("DOCBANK_API_KEY")
	if keyFile != "" {
		value, err := os.ReadFile(keyFile) // #nosec G304 -- Explicit operator-selected credential file.
		if err != nil {
			return "", fmt.Errorf("reading API key file: %w", err)
		}
		key = strings.TrimSpace(string(value))
	}
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return "", errors.New("set DOCBANK_API_KEY or --api-key-file to a nonempty API key")
	}
	return key, nil
}

func (c ConnectionConfig) validate() error {
	// Bound user input before selection compilation/JSON allocation. Each JSON
	// element/record has additional overhead checked again before publication.
	size := len(c.Name) + len(c.SourceRoot) + len(c.Destination) + len(c.LocalVaultRoot) + len(c.RemoteOrigin) + len(c.KeyFile)
	if len(c.Exclude) > maxRegistryBytes/2 || size > maxRegistryBytes {
		return errors.New("connection configuration exceeds registry limit")
	}
	for _, exclude := range c.Exclude {
		if !utf8.ValidString(exclude) || strings.ContainsRune(exclude, 0) {
			return errors.New("exclusion must be valid text")
		}
		if len(exclude) > maxRegistryBytes-size {
			return errors.New("connection configuration exceeds registry limit")
		}
		size += len(exclude)
	}
	if err := store.ValidatePushName(c.Name); err != nil {
		return err
	}
	if !canonicalLocalPath(c.SourceRoot) {
		return localPathError("source", c.SourceRoot)
	}
	if (c.LocalVaultRoot == "") == (c.RemoteOrigin == "") {
		return errors.New("select exactly one local vault root or remote origin")
	}
	if c.LocalVaultRoot != "" {
		if !canonicalLocalPath(c.LocalVaultRoot) {
			return localPathError("vault", c.LocalVaultRoot)
		}
		if c.KeyFile != "" {
			return errors.New("remove the key file for a local daemon")
		}
	} else {
		// Match NewPushConnection's accepted origin shape without allocating
		// a transport or rewriting the selected origin through browser policy.
		origin, err := url.Parse(c.RemoteOrigin)
		if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
			return errors.New("remote target must be an HTTP(S) origin without credentials, path, query or fragment")
		}
		if c.KeyFile != "" && !canonicalLocalPath(c.KeyFile) {
			return localPathError("key-file", c.KeyFile)
		}
	}
	if err := store.ValidatePushDestination(c.Destination); err != nil {
		return err
	}
	if path.Clean(c.Destination) != c.Destination {
		return errors.New("remove repeated or trailing slashes and . or .. from --dest")
	}
	segments := strings.Split(strings.TrimPrefix(c.Destination, "/"), "/")
	changed := false
	for i, segment := range segments {
		if segment == "" {
			continue
		}
		normalized, err := store.NormalizeName(segment)
		if err != nil {
			return errors.New("destination names must be canonical")
		}
		changed = changed || normalized != segment
		segments[i] = normalized
	}
	if changed {
		suggestion := "/" + strings.Join(segments, "/")
		if len(suggestion) <= 4096 {
			return fmt.Errorf("use --dest %q", suggestion)
		}
		return errors.New("destination names must be canonical")
	}
	if c.SettleTime <= 0 || c.MinimumAge < 0 || c.ScanInterval <= 0 {
		return errors.New("settle time and scan interval must be positive; minimum age must be nonnegative")
	}

	return ingest.ValidateOptions(ingest.Options{Exclude: c.Exclude})
}

func canonicalLocalPath(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, 0) && filepath.IsAbs(value) && filepath.Clean(value) == value
}

func localPathError(label, value string) error {
	if value == "" || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return fmt.Errorf("%s path must be nonempty valid text without NUL", label)
	}
	// Cleaning .. across a symlink may name a different location. Ask the
	// operator for a canonical path rather than suggesting a different target.
	for part := range strings.SplitSeq(filepath.ToSlash(value), "/") {
		if part == ".." {
			return fmt.Errorf("%s path must be a canonical absolute path without ..", label)
		}
	}
	absolute, err := filepath.Abs(value)
	if err != nil || len(absolute) > 4096 {
		return fmt.Errorf("%s path must be a canonical absolute path", label)
	}
	return fmt.Errorf("use %s path %q", label, absolute)
}
