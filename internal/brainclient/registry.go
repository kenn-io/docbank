package brainclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/safefileio"
)

const maxRegistryBytes = 1 << 20

var registryDurationMarshalers = json.WithMarshalers(json.MarshalFunc(func(value time.Duration) ([]byte, error) {
	return json.Marshal(int64(value))
}))

var registryDurationUnmarshalers = json.WithUnmarshalers(json.UnmarshalFunc(func(data []byte, value *time.Duration) error {
	var nanos int64
	if err := json.Unmarshal(data, &nanos); err != nil {
		return err
	}
	*value = time.Duration(nanos)
	return nil
}))

// ErrBusy means a connection is running or another process is changing it.
var ErrBusy = errors.New("connection is running or being updated")

// Registry owns only private named client settings. Its stable lock files are
// separate from the atomically replaced JSON and never removed by disconnect.
type Registry struct {
	root      string
	writeFile func(string, []byte, ...atomicfile.Option) error
}

type registryRecord struct {
	Version     int                `json:"version"`
	Connections []ConnectionConfig `json:"connections"`
}

// OpenRegistry pins an absolute registry root without creating it. A missing
// root is valid for read-only status; first mutation creates private files.
func OpenRegistry(root string) (*Registry, error) {
	if root == "" {
		return nil, errors.New("client registry root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolving client registry: %w", err)
	}
	if !canonicalLocalPath(absolute) {
		return nil, errors.New("client registry root must be a valid absolute path")
	}
	if err = safefileio.ValidatePrivateDir(absolute); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("validating client registry: %w", err)
	}
	return &Registry{root: absolute, writeFile: atomicfile.WriteFile}, nil
}

// List returns one bounded strict snapshot sorted by name. Missing registries
// read as empty, without creating directories or locks.
func (r *Registry) List(ctx context.Context) ([]ConnectionConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := safefileio.ValidatePrivateDir(r.root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []ConnectionConfig{}, nil
		}
		return nil, fmt.Errorf("validating client registry: %w", err)
	}
	file, err := safefileio.OpenCurrentUserFile(filepath.Join(r.root, "connections.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return []ConnectionConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening client registry: %w", err)
	}
	defer func() { _ = file.Close() }()
	if err := safefileio.ValidatePrivateCurrentUserFile(file); err != nil {
		return nil, fmt.Errorf("validating client registry file: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxRegistryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading client registry: %w", err)
	}
	if len(data) > maxRegistryBytes {
		return nil, errors.New("client registry exceeds 1 MiB")
	}
	var record registryRecord
	if err = json.Unmarshal(data, &record, json.RejectUnknownMembers(true), registryDurationUnmarshalers); err != nil {
		return nil, errors.New("invalid client registry JSON")
	}
	if record.Version != 1 || record.Connections == nil {
		return nil, errors.New("unsupported or incomplete client registry")
	}
	previous := ""
	for i, cfg := range record.Connections {
		if err := cfg.validate(); err != nil {
			return nil, fmt.Errorf("invalid client connection at index %d: %w", i, err)
		}
		if i > 0 && cfg.Name <= previous {
			return nil, errors.New("client connections must have unique sorted names")
		}
		previous = cfg.Name
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return record.Connections, nil
}

// Get reads one named connection. The returned record owns its slices.
func (r *Registry) Get(ctx context.Context, name string) (ConnectionConfig, error) {
	if err := store.ValidatePushName(name); err != nil {
		return ConnectionConfig{}, err
	}
	list, err := r.List(ctx)
	if err != nil {
		return ConnectionConfig{}, err
	}
	for _, cfg := range list {
		if cfg.Name == name {
			return cfg, nil
		}
	}
	return ConnectionConfig{}, fmt.Errorf("connection not found: %w", fs.ErrNotExist)
}

// Put creates a connection or accepts an identical saved configuration.
// Changed settings require an explicit disconnect; active names cannot mutate.
func (r *Registry) Put(ctx context.Context, cfg ConnectionConfig) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	run, err := r.lockName(ctx, cfg.Name)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, run.Close()) }()
	lock, err := r.lockRegistry(ctx)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, lock.Close()) }()
	list, err := r.List(ctx)
	if err != nil {
		return err
	}
	for _, saved := range list {
		if saved.Name == cfg.Name {
			if equalConfig(saved, cfg) {
				return nil
			}
			return fmt.Errorf("connection already exists with different settings: %w", fs.ErrExist)
		}
	}
	list = append(list, cfg)
	slices.SortFunc(list, func(a, b ConnectionConfig) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return r.save(ctx, list)
}

// Remove deletes only a stopped connection's client settings. It does not
// contact a daemon or change archived/publication authority.
func (r *Registry) Remove(ctx context.Context, name string) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := r.Get(ctx, name); err != nil {
		return err
	}
	run, err := r.lockName(ctx, name)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, run.Close()) }()
	lock, err := r.lockRegistry(ctx)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, lock.Close()) }()
	list, err := r.List(ctx)
	if err != nil {
		return err
	}
	for i, cfg := range list {
		if cfg.Name == name {
			return r.save(ctx, slices.Delete(list, i, i+1))
		}
	}
	return fmt.Errorf("connection not found: %w", fs.ErrNotExist)
}

// AcquireRun excludes other runs and changes to this name until release.
// Release is idempotent and leaves the stable lock file in place.
func (r *Registry) AcquireRun(ctx context.Context, name string) (func() error, error) {
	lock, err := r.lockName(ctx, name)
	if err != nil {
		return nil, err
	}
	if _, err = r.Get(ctx, name); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	return lock.Close, nil
}

func (r *Registry) lockName(ctx context.Context, name string) (*flock.Flock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := store.ValidatePushName(name); err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(name))
	return r.lock(ctx, "run-"+hex.EncodeToString(hash[:])+".lock", false)
}
func (r *Registry) lockRegistry(ctx context.Context) (*flock.Flock, error) {
	return r.lock(ctx, "registry.lock", true)
}

func (r *Registry) lock(ctx context.Context, name string, wait bool) (*flock.Flock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := safefileio.EnsurePrivateDir(r.root); err != nil {
		return nil, fmt.Errorf("preparing private client registry: %w", err)
	}
	target := filepath.Join(r.root, name)
	file, err := safefileio.CreatePrivateFile(target)
	if errors.Is(err, fs.ErrExist) {
		file, err = safefileio.OpenCurrentUserFile(target)
	}
	if err != nil {
		return nil, fmt.Errorf("opening client lock: %w", err)
	}
	validErr := safefileio.ValidatePrivateCurrentUserFile(file)
	if err = errors.Join(validErr, file.Close()); err != nil {
		return nil, fmt.Errorf("validating client lock: %w", err)
	}
	// The validated private directory is the trust boundary. Open only the
	// existing stable regular lock, never a new file through flock's default.
	lock := flock.New(target, flock.SetFlag(os.O_RDWR))
	var held bool
	if wait {
		held, err = lock.TryLockContext(ctx, 25*time.Millisecond)
	} else {
		held, err = lock.TryLock()
	}
	if err != nil || !held {
		if err == nil {
			err = ErrBusy
		}
		return nil, errors.Join(err, lock.Close())
	}
	if err = ctx.Err(); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	return lock, nil
}

func equalConfig(a, b ConnectionConfig) bool {
	exclusionsEqual := slices.Equal(a.Exclude, b.Exclude)
	return exclusionsEqual && a.Name == b.Name && a.SourceRoot == b.SourceRoot && a.Destination == b.Destination && a.LocalVaultRoot == b.LocalVaultRoot && a.RemoteOrigin == b.RemoteOrigin && a.KeyFile == b.KeyFile && a.SettleTime == b.SettleTime && a.MinimumAge == b.MinimumAge && a.ScanInterval == b.ScanInterval && a.IndexLocal == b.IndexLocal
}

func (r *Registry) save(ctx context.Context, list []ConnectionConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(registryRecord{Version: 1, Connections: list}, registryDurationMarshalers)
	if err != nil {
		return errors.New("encoding client registry failed")
	}
	if len(data) > maxRegistryBytes {
		return errors.New("client registry exceeds 1 MiB")
	}
	err = r.writeFile(filepath.Join(r.root, "connections.json"), data, atomicfile.WithPrivate())
	if !errors.Is(err, atomicfile.ErrPublished) {
		return err
	}
	// Publication already occurred. Read back visible state while still holding
	// both locks and report uncertainty; never roll back or silently retry.
	visible, readErr := r.List(ctx)
	if readErr != nil {
		return errors.Join(err, fmt.Errorf("reading published client registry: %w", readErr))
	}
	if !slices.EqualFunc(list, visible, equalConfig) {
		return errors.Join(err, errors.New("published client registry differs from requested state"))
	}
	return err
}
