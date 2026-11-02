package backup

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/kit/atomicfile"
)

// LoadHashMapCache reads the disposable local hash-map cache. Any read or
// parse failure returns empty results, never an error: the cache is rebuilt
// from the repository when unusable. A repoID that is not a canonical
// generated ID is an error, not a miss — it is joined into cacheDir as a
// filename, so anything else could address a file outside the cache.
func LoadHashMapCache(cacheDir, repoID string) (string, *PageHashMap, error) {
	if !validRepoID(repoID) {
		return "", nil, fmt.Errorf(
			"backup: cache key %q is not a canonical repository ID", repoID)
	}
	data, err := os.ReadFile(filepath.Join(cacheDir, repoID+".hashmap"))
	if err != nil {
		return "", nil, nil //nolint:nilerr // absent/unreadable cache is a cache miss by design
	}
	if len(data) < 4 {
		return "", nil, nil
	}
	n := binary.LittleEndian.Uint32(data[:4])
	if uint64(len(data)) < 4+uint64(n) {
		return "", nil, nil
	}
	snapshotID := string(data[4 : 4+n])
	m, err := DecodeHashKeyframe(data[4+n:])
	if err != nil {
		return "", nil, nil //nolint:nilerr // corrupt cache is a cache miss by design
	}
	return snapshotID, m, nil
}

// SaveHashMapCache atomically replaces the local hash-map cache. repoID must
// be a canonical generated repository ID: it becomes a filename under
// cacheDir, so any other value could write outside the cache. An empty
// cacheDir means the cache is disabled — the convention loading already
// uses — so saving is an explicit successful no-op rather than an accident
// of empty-path handling (MkdirAll("") happens to fail).
func SaveHashMapCache(cacheDir, repoID, snapshotID string, m *PageHashMap) error {
	if cacheDir == "" {
		return nil
	}
	if !validRepoID(repoID) {
		return fmt.Errorf(
			"backup: cache key %q is not a canonical repository ID", repoID)
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return fmt.Errorf("backup: creating cache dir: %w", err)
	}
	buf := binary.LittleEndian.AppendUint32(nil, uint32(len(snapshotID)))
	buf = append(buf, snapshotID...)
	buf = append(buf, EncodeHashKeyframe(m)...)
	// The cache is disposable, so the write skips fsync: a crash may lose
	// it, but readers never see a partial file.
	if err := atomicfile.WriteFile(
		filepath.Join(cacheDir, repoID+".hashmap"), buf,
		atomicfile.WithPerm(0o600), atomicfile.WithoutSync(),
	); err != nil {
		return fmt.Errorf("backup: publishing cache: %w", err)
	}
	return nil
}
