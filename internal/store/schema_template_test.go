package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.FileExists(t, dbPath)

	s, err := Open(t.Context(), dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	require.NoError(t, validateUUIDv4(s.VaultID()))
	var version int
	require.NoError(t, s.db.QueryRowContext(t.Context(),
		`SELECT schema_version FROM vault_metadata WHERE singleton = 1`).Scan(&version))
	assert.Equal(t, currentStorageSchemaVersion, version)
}
