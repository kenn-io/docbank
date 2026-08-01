package backupapp

import (
	"errors"
	"fmt"
	"io"

	"github.com/BurntSushi/toml"
)

const maxRestoreStoreMapBytes int64 = 1 << 20

// LoadRestoreStoreMap reads one owner-private, no-follow TOML mapping file.
// The file carries binding names only; endpoints and credentials stay in the
// daemon's config.toml.
func LoadRestoreStoreMap(path string) (RestoreStoreMap, error) {
	file, err := openRestoreStoreMap(path)
	if err != nil {
		return RestoreStoreMap{}, fmt.Errorf("opening restore store map: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return RestoreStoreMap{}, fmt.Errorf("inspecting restore store map: %w", err)
	}
	if !info.Mode().IsRegular() {
		return RestoreStoreMap{}, errors.New("restore store map is not a regular file")
	}
	if info.Size() < 0 || info.Size() > maxRestoreStoreMapBytes {
		return RestoreStoreMap{}, fmt.Errorf(
			"restore store map exceeds %d bytes", maxRestoreStoreMapBytes,
		)
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxRestoreStoreMapBytes+1))
	if err != nil {
		return RestoreStoreMap{}, fmt.Errorf("reading restore store map: %w", err)
	}
	if int64(len(raw)) > maxRestoreStoreMapBytes {
		return RestoreStoreMap{}, fmt.Errorf(
			"restore store map exceeds %d bytes", maxRestoreStoreMapBytes,
		)
	}
	var mapping RestoreStoreMap
	md, err := toml.Decode(string(raw), &mapping)
	if err != nil {
		return RestoreStoreMap{}, fmt.Errorf("decoding restore store map: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return RestoreStoreMap{}, fmt.Errorf(
			"restore store map contains unknown key %q", undecoded[0].String(),
		)
	}
	return mapping, nil
}
