package store

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	docsqlite "go.kenn.io/docbank/sqlite"
	"go.kenn.io/docbank/sqlite/modernc"
)

func TestSourceMetadataGenerationsAreImmutableAndAttachmentFactsStayJoined(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	ingest, err := s.BeginIngest(ctx, "cli", "/synthetic")
	require.NoError(t, err)
	node, _, err := s.IngestFile(ctx, ingest, s.RootID(), "report.PDF", fakeHash("a1"), 12,
		"application/pdf", "/synthetic/report.PDF", "2024-01-02T03:04:05Z")
	require.NoError(t, err)
	version, err := s.ContentVersionByID(ctx, node.CurrentVersionID)
	require.NoError(t, err)

	metadata := document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1,
		Fields: []document.SourceMetadataFieldV1{{Key: "title", Namespace: "pdf.info", SourceField: "Title",
			Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: "Report"}}}}
	canonical, _, err := document.MarshalSourceMetadataV1(metadata)
	require.NoError(t, err)
	generation, err := s.PublishSourceMetadata(ctx, version.BlobHash, fakeHash("f1"), canonical)
	require.NoError(t, err)
	assert.Equal(t, version.BlobHash, generation.SourceSHA256)

	view, err := s.ContentVersionSourceMetadata(ctx, version.ID)
	require.NoError(t, err)
	assert.Equal(t, "Report", view.Metadata.Fields[0].Value.String)
	assert.Equal(t, "report.PDF", view.Attachment.Filename)
	assert.Equal(t, ".pdf", view.Attachment.Extension)
	assert.Equal(t, "/synthetic/report.PDF", view.Attachment.SourcePath)
	assert.NotContains(t, string(view.Generation.CanonicalJSON), "report.PDF")

	changed := metadata
	changed.Fields[0].Value.String = "Changed"
	changedCanonical, _, err := document.MarshalSourceMetadataV1(changed)
	require.NoError(t, err)
	_, err = s.PublishSourceMetadata(ctx, version.BlobHash, fakeHash("f1"), changedCanonical)
	require.ErrorContains(t, err, "different evidence")

	other, err := s.CreateFile(ctx, s.RootID(), "other.pdf", fakeHash("b2"), 4, "application/pdf")
	require.NoError(t, err)
	otherGeneration, err := s.PublishSourceMetadata(ctx, other.BlobHash, fakeHash("f1"), canonical)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE source_metadata_heads SET generation_id=? WHERE source_sha256=?`,
		otherGeneration.GenerationID, version.BlobHash)
	require.Error(t, err, "a head must not select another original's evidence")
}

func TestSourceMetadataJSONLRoundTripsAcrossSQLiteDrivers(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		driver docsqlite.Driver
	}{{"default", DefaultSQLiteDriver()}, {"pure Go", modernc.Driver{}}} {
		t.Run(testCase.name, func(t *testing.T) {
			source, err := Open(filepath.Join(t.TempDir(), "source.db"), testCase.driver)
			require.NoError(t, err)
			defer func() { require.NoError(t, source.Close()) }()
			node, err := source.CreateFile(t.Context(), source.RootID(), "report.pdf", fakeHash("a1"), 4, "application/pdf")
			require.NoError(t, err)
			canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1,
				Fields: []document.SourceMetadataFieldV1{{Key: "title", Namespace: "pdf.info", SourceField: "Title", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: "Round\u2028trip"}}}})
			require.NoError(t, err)
			generation, err := source.PublishSourceMetadata(t.Context(), node.BlobHash, fakeHash("f1"), canonical)
			require.NoError(t, err)
			var exported bytes.Buffer
			require.NoError(t, source.ExportMetadata(t.Context(), &exported))
			assert.Contains(t, exported.String(), `"type":"source_metadata_generation"`)
			target, err := Open(filepath.Join(t.TempDir(), "target.db"), testCase.driver)
			require.NoError(t, err)
			defer func() { require.NoError(t, target.Close()) }()
			require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
			restored, metadata, err := target.ActiveSourceMetadata(t.Context(), node.BlobHash)
			require.NoError(t, err)
			assert.Equal(t, generation.GenerationID, restored.GenerationID)
			assert.Equal(t, "Round\u2028trip", metadata.Fields[0].Value.String)
		})
	}
}

func TestMissingSourceMetadataTargetsAreFingerprintScopedAndResumable(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	ingest, err := s.BeginIngest(ctx, "cli", "/synthetic")
	require.NoError(t, err)
	_, _, err = s.IngestFile(ctx, ingest, s.RootID(), "one.pdf", fakeHash("a1"), 3,
		"application/pdf", "/synthetic/one.pdf", "")
	require.NoError(t, err)

	targets, err := s.MissingSourceMetadataTargets(ctx, fakeHash("f1"), 10)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{
		ContractVersion: document.SourceMetadataContractV1})
	require.NoError(t, err)
	_, err = s.PublishSourceMetadata(ctx, targets[0].SourceSHA256, fakeHash("f1"), canonical)
	require.NoError(t, err)
	targets, err = s.MissingSourceMetadataTargets(ctx, fakeHash("f1"), 10)
	require.NoError(t, err)
	assert.Empty(t, targets)
	targets, err = s.MissingSourceMetadataTargets(ctx, fakeHash("f2"), 10)
	require.NoError(t, err)
	assert.Len(t, targets, 1)
}

func TestMissingSourceMetadataTargetsCanAdvancePastAFailingHash(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	for index, hash := range []string{fakeHash("a1"), fakeHash("b2"), fakeHash("c3")} {
		_, err := s.CreateFile(ctx, s.RootID(), "source-"+string(rune('a'+index))+".pdf", hash, 4, "application/pdf")
		require.NoError(t, err)
	}

	first, err := s.MissingSourceMetadataTargetsAfter(ctx, fakeHash("f1"), "", 1)
	require.NoError(t, err)
	require.Equal(t, []SourceMetadataTarget{{SourceSHA256: fakeHash("a1"), Size: 4}}, first)
	second, err := s.MissingSourceMetadataTargetsAfter(ctx, fakeHash("f1"), first[0].SourceSHA256, 1)
	require.NoError(t, err)
	assert.Equal(t, []SourceMetadataTarget{{SourceSHA256: fakeHash("b2"), Size: 4}}, second)
}
