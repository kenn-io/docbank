package posthog

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"go.kenn.io/kit/atomicfile"
)

// InstallFileName is the file LoadOrCreateInstall keeps in its directory.
const InstallFileName = "telemetry-install.json"

var errInvalidInstall = errors.New("invalid telemetry install file")

// Install is an anonymous installation identity for Options.DistinctID and
// Options.InstalledAt.
type Install struct {
	ID          string    `json:"install_id"`
	InstalledAt time.Time `json:"installed_at"`
}

// LoadOrCreateInstall returns the install stored in dir, creating
// InstallFileName as a private file on first use. A file that does not parse
// is replaced with a new install. Every call holds a lock file beside it, so
// processes that share dir get the same install.
func LoadOrCreateInstall(dir string) (Install, error) {
	path := filepath.Join(dir, InstallFileName)
	// Reads take the lock too: Windows can't replace a file another caller has open.
	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		return Install{}, fmt.Errorf("lock telemetry install file: %w", err)
	}
	defer func() { _ = lock.Unlock() }()
	inst, err := readInstall(path)
	if err == nil {
		return inst, nil
	}
	if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, errInvalidInstall) {
		return Install{}, fmt.Errorf("read telemetry install file: %w", err)
	}
	inst = Install{ID: rand.Text(), InstalledAt: time.Now().UTC()}
	data, err := json.Marshal(inst)
	if err != nil {
		return Install{}, fmt.Errorf("encode telemetry install file: %w", err)
	}
	if err := atomicfile.WriteFile(path, data, atomicfile.WithPrivate()); err != nil {
		return Install{}, fmt.Errorf("write telemetry install file: %w", err)
	}
	return inst, nil
}

func readInstall(path string) (Install, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Install{}, err
	}
	var inst Install
	if err := json.Unmarshal(data, &inst); err != nil || strings.TrimSpace(inst.ID) == "" {
		return Install{}, fmt.Errorf("%w %s", errInvalidInstall, path)
	}
	return inst, nil
}
