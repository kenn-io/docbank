package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const frozenTestSchema = `
CREATE TABLE notes (id INTEGER PRIMARY KEY, created_at TEXT);
CREATE TABLE blobs (
  id INTEGER PRIMARY KEY,
  content_hash TEXT,
  storage_path TEXT,
  size INTEGER,
  preview_hash TEXT,
  preview_path TEXT
);
`

func newFrozenTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(t.Context(), frozenTestSchema)
	require.NoError(t, err)
	seed := `
INSERT INTO notes (created_at) VALUES ('2026-01-01T00:00:00Z'), ('2026-02-01T00:00:00Z');
INSERT INTO blobs (content_hash, storage_path, size, preview_hash, preview_path) VALUES
  ('aa11', 'aa/aa11', 100, 'tt77', 'tt/tt77'),
  ('aa11', 'aa/aa11', 100, '', ''),
  ('bb22', 'bb/bb22', 50, '', ''),
  ('cc33', 'https://example.com/x', 5, '', ''),
  ('dd44', 'dd/dd44', NULL, '', ''),
  ('ee55', 'http-cache/ee/ee55', 25, '', ''),
  ('', '', 0, '', '');
`
	_, err = db.ExecContext(t.Context(), seed)
	require.NoError(t, err)
	return path, db
}

func TestFrozenSessionPinsAndCounts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	path, writer := newFrozenTestDB(t)
	s, err := OpenFrozenSession(ctx, path, NoopFreezeCoordinator{})
	require.NoError(err)
	defer func() { require.NoError(s.Close()) }()

	assert.Positive(s.PageSize)
	assert.Positive(s.PageCount)

	// WAL was checkpoint-truncated at open.
	walInfo, statErr := os.Stat(path + "-wal")
	if statErr == nil {
		assert.Zero(walInfo.Size(), "WAL should be truncated")
	}

	// A concurrent writer proceeds (into the WAL) while the session is open.
	_, err = writer.ExecContext(ctx, `INSERT INTO notes (created_at) VALUES ('2026-03-01T00:00:00Z')`)
	require.NoError(err)

	// The pinned snapshot does not see the new row. The frozen Tx is the
	// engine's contract; a production App's stats/content queries over it are
	// covered by that application's own tests.
	var notes int64
	require.NoError(s.Tx().QueryRowContext(ctx, "SELECT COUNT(*) FROM notes").Scan(&notes))
	assert.Equal(int64(2), notes, "the pinned snapshot must not see the concurrently written row")
}

// TestFrozenSessionOpensPathWithQueryChars pins the session's DSN
// construction: a database path containing '?' (legal on POSIX filesystems)
// must open that file itself. A naive path+"?params" DSN would truncate at
// the first '?', treat the filename's tail as connection parameters, and
// silently snapshot a freshly created empty database instead of the archive.
func TestFrozenSessionOpensPathWithQueryChars(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("'?' is not a legal filename character on Windows")
	}
	require := require.New(t)
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "odd? archive#1.db")
	db, err := sql.Open("sqlite3", sqliteURIDSN(path, "_journal_mode=WAL&_busy_timeout=5000"))
	require.NoError(err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(t.Context(), frozenTestSchema)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO notes (created_at) VALUES ('2026-01-01T00:00:00Z')`)
	require.NoError(err)

	s, err := OpenFrozenSession(ctx, path, NoopFreezeCoordinator{})
	require.NoError(err)
	defer func() { require.NoError(s.Close()) }()
	var notes int64
	require.NoError(s.Tx().QueryRowContext(ctx, "SELECT COUNT(*) FROM notes").Scan(&notes))
	require.Equal(int64(1), notes,
		"the session must pin the file at the odd path, not an empty side database")
}

// TestFrozenSessionRefusesMissingDatabase pins the mode=rw open: a missing or
// typo'd DBPath must fail the capture loudly, never have SQLite's default rwc
// create an empty database that capture would then "succeed" over.
func TestFrozenSessionRefusesMissingDatabase(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "does-not-exist.db")

	_, err := OpenFrozenSession(t.Context(), path, NoopFreezeCoordinator{})
	require.Error(err)

	_, statErr := os.Stat(path)
	require.True(os.IsNotExist(statErr), "capture must not create the missing database file")
}

func TestFrozenSessionCoordinatorErrors(t *testing.T) {
	require := require.New(t)
	path, _ := newFrozenTestDB(t)
	fc := &recordingCoordinator{beginErr: assert.AnError}
	_, err := OpenFrozenSession(t.Context(), path, fc)
	require.ErrorIs(err, assert.AnError)

	fc = &recordingCoordinator{}
	s, err := OpenFrozenSession(t.Context(), path, fc)
	require.NoError(err)
	require.NoError(s.Close())
	require.True(fc.began)
	require.True(fc.ended, "gate must be released after the freeze window")
}

// TestFrozenSessionJoinsPinErrorAndEndError pins the fix ensuring
// OpenFrozenSession still calls fc.End even when openPinnedSession fails, and
// surfaces End's error alongside the pin failure instead of discarding it.
// dbPath is a directory (not a sqlite file), which fails openPinnedSession's
// first query deterministically and quickly.
func TestFrozenSessionJoinsPinErrorAndEndError(t *testing.T) {
	require := require.New(t)
	dbPath := t.TempDir()
	fc := &recordingCoordinator{endErr: assert.AnError}

	_, err := OpenFrozenSession(t.Context(), dbPath, fc)
	require.Error(err)
	require.True(fc.began)
	require.True(fc.ended, "End must still be called after a pin failure")
	require.ErrorIs(err, assert.AnError, "End's error must surface, not be dropped")
	require.ErrorContains(err, "unable to open database file",
		"the pin failure must still surface alongside End's error")
}

type recordingCoordinator struct {
	beginErr error
	endErr   error
	began    bool
	ended    bool
}

func (c *recordingCoordinator) Begin(context.Context) error {
	c.began = true
	return c.beginErr
}

func (c *recordingCoordinator) End(context.Context) error {
	c.ended = true
	return c.endErr
}
