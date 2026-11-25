// Package providers keeps the person's hosted-vault provider connections in a
// user-level file outside every vault root. Each connection names the hosting
// service and the trusted local helper that answers for it.
package providers

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"uuid"

	"github.com/BurntSushi/toml"
	"github.com/gofrs/flock"
	"go.kenn.io/kit/atomicfile"

	"go.kenn.io/docbank/docbank/provider"
)

// ConfigHomeEnv overrides the directory that holds providers.toml.
const ConfigHomeEnv = "DOCBANK_CONFIG_HOME"

const fileName = "providers.toml"

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// Provider is one configured hosting service.
type Provider struct {
	Origin        string   `toml:"origin" json:"origin"`
	MountPrefix   string   `toml:"mount_prefix" json:"mount_prefix"`
	BrowserOrigin string   `toml:"browser_origin" json:"browser_origin"`
	Helper        []string `toml:"helper" json:"helper"`
}

// File is the saved configuration. InstallationID identifies this Docbank
// installation to every provider and survives reconfiguration.
type File struct {
	InstallationID uuid.UUID           `toml:"installation_id"`
	Providers      map[string]Provider `toml:"providers"`
}

// Store reads and writes providers.toml in one directory.
type Store struct {
	dir string
}

// DefaultStore uses $DOCBANK_CONFIG_HOME, or a docbank directory in the
// operating system's user configuration directory.
func DefaultStore() (Store, error) {
	if dir := os.Getenv(ConfigHomeEnv); dir != "" {
		if !filepath.IsAbs(dir) {
			return Store{}, fmt.Errorf("%s must be an absolute path", ConfigHomeEnv)
		}
		return Store{dir: filepath.Clean(dir)}, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return Store{}, fmt.Errorf("locating the user configuration directory: %w", err)
	}
	return Store{dir: filepath.Join(base, "docbank")}, nil
}

// Path is the configuration file's location.
func (s Store) Path() string { return filepath.Join(s.dir, fileName) }

// Load reads the configuration. A missing file is an empty configuration.
func (s Store) Load() (File, error) {
	data, err := os.ReadFile(s.Path())
	if errors.Is(err, fs.ErrNotExist) {
		return File{Providers: map[string]Provider{}}, nil
	}
	if err != nil {
		return File{}, fmt.Errorf("reading %s: %w", s.Path(), err)
	}
	var file File
	meta, err := toml.Decode(string(data), &file)
	if err != nil {
		return File{}, fmt.Errorf("reading %s: %w", s.Path(), err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return File{}, fmt.Errorf("reading %s: unknown setting %s", s.Path(), undecoded[0])
	}
	if file.Providers == nil {
		file.Providers = map[string]Provider{}
	}
	for _, name := range slices.Sorted(maps.Keys(file.Providers)) {
		if err := Validate(name, file.Providers[name]); err != nil {
			return File{}, fmt.Errorf("reading %s: [providers.%s]: %w", s.Path(), name, err)
		}
	}
	if len(file.Providers) > 0 && file.InstallationID == uuid.Nil() {
		return File{}, fmt.Errorf("reading %s: installation_id is missing", s.Path())
	}
	return file, nil
}

// Update applies change to the saved configuration under a lock and replaces
// the file atomically. When change fails, the file stays as it was. Hold the
// lock only for the change itself: a slow step such as asking a helper belongs
// before Update, so it cannot stall other setups.
func (s Store) Update(change func(*File) error) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", s.dir, err)
	}
	lock := flock.New(filepath.Join(s.dir, fileName+".lock"))
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("locking %s: %w", s.Path(), err)
	}
	defer func() { _ = lock.Unlock() }()
	file, err := s.Load()
	if err != nil {
		return err
	}
	if err := change(&file); err != nil {
		return err
	}
	var data bytes.Buffer
	if err := toml.NewEncoder(&data).Encode(file); err != nil {
		return fmt.Errorf("encoding %s: %w", s.Path(), err)
	}
	if err := atomicfile.WriteFile(s.Path(), data.Bytes(), atomicfile.WithPrivate()); err != nil {
		return fmt.Errorf("writing %s: %w", s.Path(), err)
	}
	return nil
}

// Validate checks one provider and its name.
func Validate(name string, p Provider) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("provider name %q must be 1 to 63 lowercase letters, digits, hyphens, or underscores", name)
	}
	if err := provider.ValidateConnection(p.Origin, p.MountPrefix); err != nil {
		return err
	}
	if err := provider.ValidateOrigin(p.BrowserOrigin); err != nil {
		return fmt.Errorf("browser %w", err)
	}
	return provider.ValidateHelper(p.Helper)
}

// Connection is the identity Docbank sends to the provider's helper.
func Connection(file File, p Provider) provider.Connection {
	return provider.Connection{InstallationID: file.InstallationID, Origin: p.Origin, MountPrefix: p.MountPrefix}
}
