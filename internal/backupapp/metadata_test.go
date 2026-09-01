package backupapp

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
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
	roles := []string{
		"normalized_evidence", "sanitized_markdown",
		string(document.EvidenceArtifactImage), string(document.EvidenceArtifactMarkdown),
		string(document.EvidenceArtifactStructured), string(document.EvidenceArtifactTranscript),
	}
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
