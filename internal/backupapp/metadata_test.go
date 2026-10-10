package backupapp

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
)

func TestDerivativeAuthorityStatsAcceptEveryCatalogArtifactRole(t *testing.T) {
	db, err := store.DefaultSQLiteDriver().Open(filepath.Join(t.TempDir(), "roles.db"),
		docsqlite.OpenOptions{Access: docsqlite.Create, TransactionMode: docsqlite.Immediate})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.Exec(`
		CREATE TABLE rendition_artifacts (
			role TEXT, build_id TEXT, artifact_id TEXT, blob_hash TEXT, size INTEGER, checksum TEXT
		);
		CREATE TABLE rendition_lexical_segments (
			build_id TEXT, segment_id TEXT, checksum TEXT, text TEXT, segment_order INTEGER
		);
		CREATE TABLE visual_preview_generations (
			generation_id TEXT, output_blob_hash TEXT, output_size INTEGER,
			checksum TEXT, state TEXT
		);`)
	require.NoError(t, err)
	roles := store.PersistedRenditionArtifactRoles()
	for index, role := range roles {
		_, err = db.Exec(`INSERT INTO rendition_artifacts(
			role,build_id,artifact_id,blob_hash,size,checksum
		) VALUES(?,?,?,?,?,?)`, role, "build", role, role+"-blob", index+1, role+"-checksum")
		require.NoError(t, err)
	}
	_, err = db.Exec(`INSERT INTO visual_preview_generations(
		generation_id,output_blob_hash,output_size,checksum,state
	) VALUES('preview-generation','preview-blob',17,'preview-checksum','ready')`)
	require.NoError(t, err)

	stats, present, err := computeDerivativeAuthorityStats(t.Context(), db)
	require.NoError(t, err)
	require.True(t, present)
	require.NotNil(t, stats)
	classes := make([]string, len(stats.Classes))
	for index, class := range stats.Classes {
		classes[index] = class.Class
	}
	assert.Equal(t, append(roles, "visual_preview"), classes)
}

func TestDerivativeAuthorityStatsRefuseUnregisteredRole(t *testing.T) {
	for _, role := range []string{"provider_audio", "visual_preview", "lexical_projection"} {
		t.Run(role, func(t *testing.T) {
			db, err := store.DefaultSQLiteDriver().Open(filepath.Join(t.TempDir(), "unknown-role.db"),
				docsqlite.OpenOptions{Access: docsqlite.Create, TransactionMode: docsqlite.Immediate})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			_, err = db.Exec(`CREATE TABLE rendition_artifacts (
		role TEXT, build_id TEXT, artifact_id TEXT, blob_hash TEXT, size INTEGER, checksum TEXT
	); CREATE TABLE rendition_lexical_segments (
		build_id TEXT, segment_id TEXT, checksum TEXT, text TEXT, segment_order INTEGER
	); CREATE TABLE visual_preview_generations (
		generation_id TEXT, output_blob_hash TEXT, output_size INTEGER,
		checksum TEXT, state TEXT
	); INSERT INTO rendition_artifacts(role,build_id,artifact_id,blob_hash,size,checksum)
		VALUES(?,'build','artifact','blob',1,'checksum');`, role)
			require.NoError(t, err)

			stats, present, err := computeDerivativeAuthorityStats(t.Context(), db)
			require.EqualError(t, err, fmt.Sprintf("backupapp: derivative artifact class %q is not catalog-authorized", role))
			assert.False(t, present)
			assert.Nil(t, stats)
		})
	}
}

func TestDerivativeAuthorityStatsExcludeRetainedPhotoExports(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "backup.db")
	catalog, err := store.Open(path)
	require.NoError(t, err)
	require.NoError(t, catalog.Close())
	db, err := store.DefaultSQLiteDriver().Open(path, docsqlite.OpenOptions{Access: docsqlite.Create, TransactionMode: docsqlite.Immediate})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.Exec(`INSERT INTO blobs(hash,size,created_at) VALUES('rendered',99,'2026-01-01T00:00:00Z'),('ordinary',7,'2026-01-01T00:00:00Z');
        INSERT INTO export_sources(id,owner,request_sha256,request_json,state,canonical_json,expires_at) VALUES('source','owner','digest','{}','sealed','{}','2099-01-01T00:00:00Z');
        INSERT INTO export_plans(id,owner,source_id,request_sha256,canonical_json,expires_at) VALUES('photo','owner','source','digest','{"photo_render":{"format":"png"}}','2099-01-01T00:00:00Z'),('ordinary','owner','source','digest','{}','2099-01-01T00:00:00Z');
        INSERT INTO export_role_roots(plan_id,blob_hash) VALUES('photo','rendered'),('ordinary','ordinary');`)
	require.NoError(t, err)
	stats, present, err := computeDerivativeAuthorityStats(t.Context(), db)
	require.NoError(t, err)
	require.True(t, present)
	require.Len(t, stats.Classes, 1)
	assert.Equal(t, "export_role", stats.Classes[0].Class)
	assert.Equal(t, int64(1), stats.Classes[0].Count)
	assert.Equal(t, int64(1), stats.Classes[0].BlobCount)
	assert.Equal(t, int64(7), stats.Classes[0].LogicalBytes)
}
