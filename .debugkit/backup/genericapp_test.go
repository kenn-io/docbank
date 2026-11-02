package backup_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
)

// fakeApp is a minimal non-application-specific App: one table, no content
// files. It proves kit/backup runs without any particular application schema.
type fakeApp struct{}

func (fakeApp) FrozenView(s *backup.FrozenSession) backup.FrozenView {
	return fakeView{tx: s.Tx()}
}
func (fakeApp) DBFileName() string        { return "fake.db" }
func (fakeApp) ContentDirName() string    { return "content" }
func (fakeApp) PackFileExtension() string { return ".kpack" }
func (fakeApp) RestoredContentPaths(context.Context, *sql.DB) (map[string][]string, error) {
	return nil, nil
}

func (fakeApp) RestoredStats(ctx context.Context, db *sql.DB) (json.RawMessage, error) {
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM notes").Scan(&n); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]int64{"notes": n})
}
func (fakeApp) CheckManifest(*backup.Manifest) []string { return nil }
func (fakeApp) ExcludedPaths() []string                 { return nil }
func (fakeApp) Version() string                         { return "fake-1.0" }

type fakeView struct{ tx *sql.Tx }

func (v fakeView) ContentInfo(context.Context) (*backup.ContentInfo, error) {
	return &backup.ContentInfo{}, nil
}

func (v fakeView) Stats(ctx context.Context) (json.RawMessage, error) {
	var n int64
	if err := v.tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM notes").Scan(&n); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]int64{"notes": n})
}

func TestGenericAppRoundTrip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	contentDir := filepath.Join(dataDir, "content")
	require.NoError(os.MkdirAll(contentDir, 0o700))

	dbPath := filepath.Join(dataDir, "fake.db")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT);
		INSERT INTO notes (body) VALUES ('alpha'), ('beta')`)
	require.NoError(err)
	require.NoError(db.Close())

	r, err := backup.Init(filepath.Join(base, "repo"))
	require.NoError(err)
	m, err := backup.Create(t.Context(), r, fakeApp{}, backup.CreateOptions{
		DBPath:     dbPath,
		ContentDir: contentDir,
		DataDir:    dataDir,
		CacheDir:   filepath.Join(base, "cache"),
	})
	require.NoError(err)
	assert.Equal("fake-1.0", m.AppVersion)
	assert.JSONEq(`{"notes":2}`, string(m.Stats))

	res, err := backup.Restore(t.Context(), r, fakeApp{}, backup.RestoreOptions{
		TargetDir: filepath.Join(base, "restored"),
	})
	require.NoError(err) // Restore's stats proof ran against fakeApp
	assert.Equal("fake.db", filepath.Base(res.DBPath))

	vres, err := backup.Verify(t.Context(), r, fakeApp{}, backup.VerifyOptions{})
	require.NoError(err)
	assert.Empty(vres.Problems)
}
