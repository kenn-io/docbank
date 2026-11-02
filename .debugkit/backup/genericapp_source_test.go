package backup_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
)

// fakeContentApp is a minimal non-application-specific App with one content
// table ("files"): it proves an application whose attachment bytes are never
// read from a loose directory can still back up and restore losslessly
// through ContentSource. Modeled on fakeApp above, extended with the content
// side of the App/FrozenView contract.
type fakeContentApp struct{}

func (fakeContentApp) FrozenView(s *backup.FrozenSession) backup.FrozenView {
	return fakeContentView{tx: s.Tx()}
}
func (fakeContentApp) DBFileName() string        { return "fakecontent.db" }
func (fakeContentApp) ContentDirName() string    { return "content" }
func (fakeContentApp) PackFileExtension() string { return ".kpack" }

// RestoredContentPaths re-derives the same hash -> canonical path mapping
// ContentInfo reported at capture time, but from the restored database.
func (fakeContentApp) RestoredContentPaths(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT hash FROM files ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("fakeContentApp: content path query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	paths := map[string][]string{}
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, fmt.Errorf("fakeContentApp: scanning content path: %w", err)
		}
		paths[hash] = []string{hash[:2] + "/" + hash}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fakeContentApp: content path rows: %w", err)
	}
	return paths, nil
}

func (fakeContentApp) RestoredStats(ctx context.Context, db *sql.DB) (json.RawMessage, error) {
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM files").Scan(&n); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]int64{"files": n})
}

func (fakeContentApp) CheckManifest(*backup.Manifest) []string { return nil }
func (fakeContentApp) ExcludedPaths() []string                 { return nil }
func (fakeContentApp) Version() string                         { return "fake-content-1.0" }

type fakeContentView struct{ tx *sql.Tx }

// ContentInfo reports the two content refs the "files" table records, in
// insertion order, matching what RestoredContentPaths later re-derives.
func (v fakeContentView) ContentInfo(ctx context.Context) (*backup.ContentInfo, error) {
	rows, err := v.tx.QueryContext(ctx, "SELECT hash, size FROM files ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("fakeContentView: content locator query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var refs []backup.ContentRef
	for rows.Next() {
		var ref backup.ContentRef
		if err := rows.Scan(&ref.Hash, &ref.Size); err != nil {
			return nil, fmt.Errorf("fakeContentView: scanning content locator: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fakeContentView: content locator rows: %w", err)
	}
	return &backup.ContentInfo{Refs: refs, Rows: int64(len(refs))}, nil
}

func (v fakeContentView) Stats(ctx context.Context) (json.RawMessage, error) {
	var n int64
	if err := v.tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM files").Scan(&n); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]int64{"files": n})
}

// fakeContentSource serves content bytes from memory, keyed by ref hash. It
// is a backup_test-local copy of the in-package mapSource fixture (Task 1's
// mapSource lives in package backup and is not importable from here) — the
// external-consumer proof that a real application can supply its own
// ContentSource without reaching into kit/backup internals.
type fakeContentSource struct {
	blobs map[string][]byte
	opens atomic.Int64
}

var errBlobNotInFakeSource = errors.New("fakeContentSource: blob not found")

func (s *fakeContentSource) Open(_ context.Context, ref backup.ContentRef) (io.ReadCloser, error) {
	s.opens.Add(1)
	b, ok := s.blobs[ref.Hash]
	if !ok {
		return nil, fmt.Errorf("%w: %s", errBlobNotInFakeSource, ref.Hash)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// TestGenericAppRoundTripContentSource proves the full ContentSource promise
// end to end through the public API only: an application whose attachment
// content never touches a loose directory on disk — Create captures it
// through a ContentSource, Verify finds the packs clean, and Restore
// materializes both files at their canonical "<hash[:2]>/<hash>" paths with
// exact original bytes.
func TestGenericAppRoundTripContentSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	require.NoError(os.MkdirAll(dataDir, 0o700))
	// contentDir is named for CreateOptions.ContentDir but is never created:
	// the point of this test is that Create never touches it when a
	// ContentSource is set.
	contentDir := filepath.Join(dataDir, "content")

	alpha := []byte("alpha content bytes, straight from memory")
	bravo := []byte("bravo content bytes, straight from memory")
	sumA := sha256.Sum256(alpha)
	sumB := sha256.Sum256(bravo)
	hashA := hex.EncodeToString(sumA[:])
	hashB := hex.EncodeToString(sumB[:])

	dbPath := filepath.Join(dataDir, "fakecontent.db")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `CREATE TABLE files (id INTEGER PRIMARY KEY, hash TEXT, size INTEGER);
		INSERT INTO files (hash, size) VALUES (?, ?), (?, ?)`,
		hashA, len(alpha), hashB, len(bravo))
	require.NoError(err)
	require.NoError(db.Close())

	src := &fakeContentSource{blobs: map[string][]byte{hashA: alpha, hashB: bravo}}

	r, err := backup.Init(filepath.Join(base, "repo"))
	require.NoError(err)
	m, err := backup.Create(t.Context(), r, fakeContentApp{}, backup.CreateOptions{
		DBPath:        dbPath,
		ContentDir:    contentDir,
		ContentSource: src,
		DataDir:       dataDir,
		CacheDir:      filepath.Join(base, "cache"),
	})
	require.NoError(err)
	assert.Equal("fake-content-1.0", m.AppVersion)
	assert.Equal(int64(2), m.Attachments.Blobs)
	assert.Equal(int64(2), src.opens.Load())

	_, err = os.Stat(contentDir)
	assert.True(os.IsNotExist(err), "ContentDir must never be created when ContentSource is set")

	vres, err := backup.Verify(t.Context(), r, fakeContentApp{}, backup.VerifyOptions{})
	require.NoError(err)
	assert.Empty(vres.Problems)

	target := filepath.Join(base, "restored")
	res, err := backup.Restore(t.Context(), r, fakeContentApp{}, backup.RestoreOptions{
		TargetDir: target,
	})
	require.NoError(err) // Restore's stats proof ran against fakeContentApp
	assert.Equal(int64(2), res.AttachmentBlobs)

	restoredContentDir := filepath.Join(target, "content")
	gotA, err := os.ReadFile(filepath.Join(restoredContentDir, hashA[:2], hashA))
	require.NoError(err)
	assert.Equal(alpha, gotA)
	gotB, err := os.ReadFile(filepath.Join(restoredContentDir, hashB[:2], hashB))
	require.NoError(err)
	assert.Equal(bravo, gotB)
}
