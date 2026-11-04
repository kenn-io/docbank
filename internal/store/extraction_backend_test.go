package store

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkTextPublicationPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	for name, text := range map[string]string{"unicode": "Résumé 日本語\n\tExact spacing.\n", "empty": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newPostgresQueryStore(t)
			ctx := t.Context()
			node, err := s.CreateFile(ctx, s.RootID(), "notes.txt", testSHA256([]byte(text)), int64(len(text)), "text/plain")
			require.NoError(t, err)
			require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{BlobHash: node.BlobHash, Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionFailed, Error: "synthetic first attempt"}))
			initial, err := s.MigrateLegacyPlainText(ctx)
			require.NoError(t, err)
			pending, err := s.PendingTextExtractions(ctx, 10)
			require.NoError(t, err)
			require.Equal(t, []ExtractionCandidate{{BlobHash: node.BlobHash, Size: node.Size}}, pending)
			result := ExtractionResult{BlobHash: node.BlobHash, Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionOK, Text: text}
			require.NoError(t, s.RecordExtraction(ctx, result))
			first, err := s.ActiveRendition(ctx, node.CurrentVersionID, initial.ProfileFingerprint)
			require.NoError(t, err)
			require.Len(t, first.Build.LexicalSegments, 1)
			require.Equal(t, text, first.Build.LexicalSegments[0].Text)
			var indexed string
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT text FROM rendition_lexical_index WHERE build_id=$1`, first.Build.ID).Scan(&indexed))
			require.Equal(t, text, indexed)
			second, err := s.CreateFile(ctx, s.RootID(), "copy.txt", node.BlobHash, node.Size, "text/plain")
			require.NoError(t, err)
			_, cursor, err := s.VaultChanges(ctx, "", 100)
			require.NoError(t, err)
			require.NoError(t, s.RecordExtraction(ctx, result))
			replayed, err := s.ActiveRendition(ctx, node.CurrentVersionID, initial.ProfileFingerprint)
			require.NoError(t, err)
			require.Equal(t, first.Build, replayed.Build, "re-extraction must keep the immutable build")
			require.Equal(t, first.Attachment, replayed.Attachment, "re-extraction must keep the immutable attachment")
			copied, err := s.ActiveRendition(ctx, second.CurrentVersionID, initial.ProfileFingerprint)
			require.NoError(t, err)
			require.Equal(t, first.Build, copied.Build)
			changes, _, err := s.VaultChanges(ctx, cursor, 100)
			require.NoError(t, err)
			require.Len(t, changes, 1)
			require.Equal(t, second.ID, changes[0].NodeID)
			require.Equal(t, "processed", changes[0].Kind)
			pending, err = s.PendingTextExtractions(ctx, 10)
			require.NoError(t, err)
			require.Empty(t, pending)
			checkRetentionMetadata(t, s)
		})
	}
}

func checkTextPublicationRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	ctx := t.Context()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	oldHash := seedLegacyMigrationRow(t, s, "published.txt", "81", ExtractionResult{Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionOK, Text: "already published"})
	initial, err := s.MigrateLegacyPlainText(ctx)
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, s.RootID(), "waiting.txt", fakeHash("82"), 12, "text/plain")
	require.NoError(t, err)
	_, cursor, err := s.VaultChanges(ctx, "", 100)
	require.NoError(t, err)
	var role string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
	_, err = setup.ExecContext(ctx, "REVOKE INSERT ON documents.rendition_heads FROM "+pgx.Identifier{role}.Sanitize())
	require.NoError(t, err)
	result := ExtractionResult{BlobHash: node.BlobHash, Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionOK, Text: "ready text"}
	err = s.RecordExtraction(ctx, result)
	require.ErrorContains(t, err, "publishing legacy extraction authority transition")
	require.ErrorContains(t, err, "permission denied for table rendition_heads")
	var cached int
	var generation string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM extracted_text WHERE blob_hash=$1`, node.BlobHash).Scan(&cached))
	require.Zero(t, cached, "failed publication must roll back the extracted-text cache too")
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT generation_id FROM rendition_lexical_heads WHERE singleton=1`).Scan(&generation))
	require.Equal(t, initial.LexicalGenerationID, generation)
	var builds int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM rendition_builds WHERE source_sha256<>$1`, oldHash).Scan(&builds))
	require.Zero(t, builds, "failed publication must not retain its new immutable build")
	pending, err := s.PendingTextExtractions(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, []ExtractionCandidate{{BlobHash: node.BlobHash, Size: node.Size}}, pending)
	changes, _, err := s.VaultChanges(ctx, cursor, 100)
	require.NoError(t, err)
	require.Empty(t, changes)
	_, err = setup.ExecContext(ctx, "GRANT INSERT ON documents.rendition_heads TO "+pgx.Identifier{role}.Sanitize())
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(ctx, result))
	view, err := s.ActiveRendition(ctx, node.CurrentVersionID, initial.ProfileFingerprint)
	require.NoError(t, err)
	require.Len(t, view.Build.LexicalSegments, 1)
	require.Equal(t, result.Text, view.Build.LexicalSegments[0].Text)
	pending, err = s.PendingTextExtractions(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, pending)
	checkRetentionMetadata(t, s)
}

func checkTextPurgeScopesPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	first, err := s.CreateFile(ctx, s.RootID(), "first.txt", fakeHash("83"), 11, "text/plain")
	require.NoError(t, err)
	second, err := s.CreateFile(ctx, s.RootID(), "second.txt", first.BlobHash, first.Size, "text/plain")
	require.NoError(t, err)
	_, err = s.PurgeDerivatives(ctx, PurgeRequest{ContentVersionIDs: []string{first.CurrentVersionID}})
	require.NoError(t, err)
	initial, err := s.MigrateLegacyPlainText(ctx)
	require.NoError(t, err)
	result := ExtractionResult{BlobHash: first.BlobHash, Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionOK, Text: "shared text"}
	require.NoError(t, s.RecordExtraction(ctx, result))
	_, err = s.ActiveRendition(ctx, first.CurrentVersionID, initial.ProfileFingerprint)
	require.ErrorIs(t, err, ErrNotFound)
	view, err := s.ActiveRendition(ctx, second.CurrentVersionID, initial.ProfileFingerprint)
	require.NoError(t, err)
	require.Len(t, view.Build.LexicalSegments, 1)
	require.Equal(t, result.Text, view.Build.LexicalSegments[0].Text)
	_, err = s.PurgeDerivatives(ctx, PurgeRequest{ContentVersionIDs: []string{second.CurrentVersionID}})
	require.NoError(t, err)
	// A late worker result and a later startup seed must both respect purge.
	require.NoError(t, s.RecordExtraction(ctx, result))
	require.NoError(t, s.SeedTextExtractionQueue(ctx, "plain-text", 1))
	_, err = s.MigrateLegacyPlainText(ctx)
	require.NoError(t, err)
	pending, err := s.PendingTextExtractions(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, pending)
	var cached, heads int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM extracted_text), (SELECT count(*) FROM rendition_heads)`).Scan(&cached, &heads))
	require.Zero(t, cached)
	require.Zero(t, heads)
	checkRetentionMetadata(t, s)
}

func checkTextChangedResultPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "notes.txt", fakeHash("84"), 11, "text/plain")
	require.NoError(t, err)
	result := ExtractionResult{BlobHash: node.BlobHash, Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionOK, Text: "first text"}
	require.NoError(t, s.RecordExtraction(ctx, result))
	initial, err := s.MigrateLegacyPlainText(ctx)
	require.NoError(t, err)
	before, err := s.ActiveRendition(ctx, node.CurrentVersionID, initial.ProfileFingerprint)
	require.NoError(t, err)
	require.Len(t, before.Build.LexicalSegments, 1)
	require.Equal(t, "first text", before.Build.LexicalSegments[0].Text)

	result.Text = "replacement text"
	require.NoError(t, s.RecordExtraction(ctx, result))
	after, err := s.ActiveRendition(ctx, node.CurrentVersionID, initial.ProfileFingerprint)
	require.NoError(t, err)
	require.Len(t, after.Build.LexicalSegments, 1)
	require.Equal(t, "replacement text", after.Build.LexicalSegments[0].Text)
	pending, err := s.PendingTextExtractions(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, pending, "changed results must retire the completed queue entry")
	checkRetentionMetadata(t, s)
}
