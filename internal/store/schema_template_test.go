package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	docsqlite "go.kenn.io/docbank/sqlite"
)

func TestNewVaultFollowsEmbeddedSchemaChange(t *testing.T) { //nolint:paralleltest // swaps the package-level schemaSQL
	originalSchema := schemaSQL
	t.Cleanup(func() { schemaSQL = originalSchema })
	// The first vault leaves a template of the embedded schema behind.
	first, err := Open(t.Context(), filepath.Join(t.TempDir(), "docbank.db"))
	require.NoError(t, err)
	require.NoError(t, first.Close())

	schemaSQL = schemaSQLWithAddedColumn(t, originalSchema, "blobs", "synthetic_template_column")
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "docbank.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	columns, err := queryTableColumns(t.Context(), s.db, "blobs")
	require.NoError(t, err)
	assert.Contains(t, columns, "synthetic_template_column")
}

func TestOpenBootstrapsVaultCopiedFromSchemaTemplate(t *testing.T) {
	t.Parallel()
	// Another opener can publish the template and not have bootstrapped it
	// yet when this open inspects the file.
	dbPath := filepath.Join(t.TempDir(), "docbank.db")
	createFromSchemaTemplate(t.Context(), dbPath, DefaultSQLiteDriver())
	template, err := DefaultSQLiteDriver().Open(dbPath, docsqlite.OpenOptions{
		Access: docsqlite.ReadOnlyImmutable, TransactionMode: docsqlite.Deferred,
	})
	require.NoError(t, err)
	var versionColumns, identityRows int
	require.NoError(t, template.QueryRowContext(t.Context(), `
		SELECT (SELECT COUNT(*) FROM pragma_table_info('vault_metadata') WHERE name = 'schema_version'),
		       (SELECT COUNT(*) FROM vault_metadata)`).Scan(&versionColumns, &identityRows))
	require.NoError(t, template.Close())
	require.Equal(t, 1, versionColumns, "the template must carry the versioned vault_metadata table")
	require.Zero(t, identityRows, "the template must not carry a vault identity")

	s, err := Open(t.Context(), dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	require.NoError(t, validateUUIDv4(s.VaultID()))
	var version int
	require.NoError(t, s.db.QueryRowContext(t.Context(),
		`SELECT schema_version FROM vault_metadata WHERE singleton = 1`).Scan(&version))
	assert.Equal(t, currentStorageSchemaVersion, version)
}

func TestOpenRefusesPopulatedVaultWithoutIdentity(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "docbank.db")
	s, err := Open(ctx, dbPath)
	require.NoError(t, err)
	_, err = s.Mkdir(ctx, s.RootID(), "kept")
	require.NoError(t, err)
	_, err = s.CreateTag(ctx, "kept-tag")
	require.NoError(t, err)
	_, err = s.writeDB.ExecContext(ctx, `DELETE FROM vault_metadata`)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	reopened, err := Open(ctx, dbPath)
	if reopened != nil {
		require.NoError(t, reopened.Close())
	}
	require.ErrorContains(t, err, "vault has data but no vault_metadata identity row")

	db, err := DefaultSQLiteDriver().Open(dbPath, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	var identityRows, keptDirs, keptTags int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM vault_metadata),
		       (SELECT COUNT(*) FROM nodes WHERE name = 'kept' AND kind = 'dir'),
		       (SELECT COUNT(*) FROM tags WHERE name = 'kept-tag')`).Scan(&identityRows, &keptDirs, &keptTags))
	assert.Zero(t, identityRows, "a refused open must not mint a new vault identity")
	assert.Equal(t, 1, keptDirs)
	assert.Equal(t, 1, keptTags)
}
