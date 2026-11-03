package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestDocumentEventEvidenceLoadsExactBindingAndChangesWithMTime(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	version, target := ingestDocumentEventTarget(t, s, "bound.pdf", "e81")
	snapshot, err := s.LoadDocumentEventEvidence(t.Context(), target)
	require.NoError(t, err)
	require.Equal(t, version.ID, snapshot.Target.ContentVersionID)
	require.Len(t, snapshot.Bindings, 1)
	require.Equal(t, version.ID, snapshot.Bindings[0].Binding.ContentVersionID)
	require.NotEmpty(t, snapshot.Bindings[0].EvidenceSHA256)
	require.Empty(t, snapshot.MetadataGeneration.GenerationID)
	require.NotEmpty(t, snapshot.InputsSHA256)

	before := snapshot.InputsSHA256
	rebuiltTarget := target
	rebuiltTarget.InputEpoch += 10
	rebuiltTarget.InputRevision += 10
	rebuilt, err := s.LoadDocumentEventEvidence(t.Context(), rebuiltTarget)
	require.NoError(t, err)
	require.Equal(t, before, rebuilt.InputsSHA256,
		"epoch and dirty revision are publication fences, not evidence identity")
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `DROP TRIGGER provenance_immutable_update`); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `UPDATE provenance SET original_mtime=$1 WHERE identity=$2`,
			"2024-01-02T03:04:06Z", snapshot.Bindings[0].Binding.ProvenanceIdentity)
		return err
	}))
	snapshot, err = s.LoadDocumentEventEvidence(t.Context(), target)
	require.NoError(t, err)
	require.NotEqual(t, before, snapshot.InputsSHA256)
	require.Equal(t, "2024-01-02T03:04:06Z", *snapshot.Bindings[0].OriginalMTime)
}

func TestDocumentEventEvidenceCorruptionCanBeFencedAsTerminalFailure(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "corrupt.pdf", fakeHash("e82"), 8, "application/pdf")
	require.NoError(t, err)
	metadata := document.SourceMetadataV1{
		ContractVersion: document.SourceMetadataContractV1,
		Fields: []document.SourceMetadataFieldV1{{
			Key: "created", Namespace: "pdf.info", SourceField: "CreationDate",
			Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataTimestamp,
				Timestamp: &document.SourceMetadataTimestampV1{
					Normalized: "2024-01-02", Raw: "D:20240102", Precision: "date", Timezone: "omitted",
				}},
		}},
	}
	canonical, _, err := document.MarshalSourceMetadataV1(metadata)
	require.NoError(t, err)
	generation, err := s.PublishSourceMetadata(t.Context(), node.BlobHash, fakeHash("f82"), canonical)
	require.NoError(t, err)
	require.NoError(t, s.EnsureDocumentEventRecipe(t.Context(), DocumentEventsDeriverFingerprint))
	target := requireDocumentEventTarget(t, s, node.CurrentVersionID)

	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `DROP TRIGGER source_metadata_generations_immutable_update`); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `UPDATE source_metadata_generations SET canonical_json=$1 WHERE generation_id=$2`,
			[]byte(`{"broken":`), generation.GenerationID)
		return err
	}))
	snapshot, err := s.LoadDocumentEventEvidence(t.Context(), target)
	require.ErrorIs(t, err, ErrSourceMetadataCorrupt)
	require.NotEmpty(t, snapshot.InputsSHA256, "semantic corruption still returns captured manifest proof")
	require.Equal(t, generation.GenerationID, snapshot.MetadataGeneration.GenerationID)

	require.NoError(t, s.RecordDocumentEventAttempt(t.Context(), target, snapshot.InputsSHA256,
		"failed", []byte(`[{"code":"source_metadata_corrupt"}]`)))
	targets, err := s.MissingDocumentEventTargetsAfter(t.Context(), DocumentEventsDeriverFingerprint, "", 10)
	require.NoError(t, err)
	require.Empty(t, targets, "a captured corrupt authority must not hot retry")

	_, err = s.db.ExecContext(t.Context(), `UPDATE source_metadata_generations
		SET canonical_json=?,contract_version='source-metadata/v2' WHERE generation_id=?`, canonical, generation.GenerationID)
	require.NoError(t, err)
	_, _, err = s.ActiveSourceMetadata(t.Context(), node.BlobHash)
	require.ErrorIs(t, err, ErrSourceMetadataCorrupt)

	_, err = s.PublishSourceMetadata(t.Context(), node.BlobHash, fakeHash("f82b"), mustSourceMetadata(t, "replacement"))
	require.NoError(t, err)
	err = s.RecordDocumentEventAttempt(t.Context(), target, snapshot.InputsSHA256,
		"failed", []byte(`[{"code":"source_metadata_corrupt"}]`))
	require.ErrorIs(t, err, ErrDocumentEventInputsChanged)
}

func TestDocumentEventEvidenceRetainsCompleteDigestBeyondDerivationBound(t *testing.T) {
	t.Parallel()
	t.Run("boundary", func(t *testing.T) {
		s, target := documentEventTargetWithBindings(t, document.MaxDocumentEvents-1)
		snapshot, err := s.LoadDocumentEventEvidence(t.Context(), target)
		require.NoError(t, err)
		require.Len(t, snapshot.Bindings, document.MaxDocumentEvents-1)
		require.NotEmpty(t, snapshot.InputsSHA256)
	})

	t.Run("one over is fenceable and hashes omitted suffix", func(t *testing.T) {
		s, target := documentEventTargetWithBindings(t, document.MaxDocumentEvents)
		snapshot, err := s.LoadDocumentEventEvidence(t.Context(), target)
		require.ErrorIs(t, err, ErrDocumentEventEvidenceUnavailable)
		require.Len(t, snapshot.Bindings, document.MaxDocumentEvents-1)
		require.NotEmpty(t, snapshot.InputsSHA256)

		require.NoError(t, s.RecordDocumentEventAttempt(t.Context(), target, snapshot.InputsSHA256,
			"unavailable", []byte(`[{"code":"evidence_bound"}]`)))
		lastIdentity := fakeHash(fmt.Sprintf("%04x", document.MaxDocumentEvents))
		require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(t.Context(), `DROP TRIGGER provenance_version_bindings_immutable_update`); err != nil {
				return err
			}
			_, err := tx.ExecContext(t.Context(), `UPDATE provenance_version_bindings
				SET basis_ref=$1 WHERE provenance_identity=$2 AND content_version_id=$3`,
				"changed beyond retained prefix", lastIdentity, target.ContentVersionID)
			return err
		}))
		err = s.RecordDocumentEventAttempt(t.Context(), target, snapshot.InputsSHA256,
			"unavailable", []byte(`[{"code":"evidence_bound"}]`))
		require.ErrorIs(t, err, ErrDocumentEventInputsChanged)
	})
}

func TestDocumentEventEvidenceHashesSourceMetadataAsBlobBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  func([]byte) []byte
	}{
		{
			name: "text nul suffix",
			raw: func(canonical []byte) []byte {
				return append(append([]byte(nil), canonical...), []byte("\x00suffix")...)
			},
		},
		{
			name: "multibyte split across chunk boundary",
			raw: func([]byte) []byte {
				return []byte(strings.Repeat("a", (64<<10)-1) + "é-tail")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStore(t)
			node, err := s.CreateFile(t.Context(), s.RootID(), "bytes.pdf", fakeHash("b87e"), 8, "application/pdf")
			require.NoError(t, err)
			canonical := mustSourceMetadata(t, "bytes")
			generation, err := s.PublishSourceMetadata(t.Context(), node.BlobHash, fakeHash("b87f"), canonical)
			require.NoError(t, err)
			require.NoError(t, s.EnsureDocumentEventRecipe(t.Context(), DocumentEventsDeriverFingerprint))
			target := requireDocumentEventTarget(t, s, node.CurrentVersionID)
			raw := test.raw(canonical)
			require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(t.Context(), `DROP TRIGGER source_metadata_generations_immutable_update`); err != nil {
					return err
				}
				_, err := tx.ExecContext(t.Context(), `UPDATE source_metadata_generations
					SET canonical_json=CAST($1 AS TEXT) WHERE generation_id=$2`, string(raw), generation.GenerationID)
				return err
			}))

			snapshot, err := s.LoadDocumentEventEvidence(t.Context(), target)
			require.ErrorIs(t, err, ErrSourceMetadataCorrupt)
			require.NotEmpty(t, snapshot.InputsSHA256)
			require.Equal(t, raw, snapshot.MetadataGeneration.CanonicalJSON)
			require.NoError(t, s.RecordDocumentEventAttempt(t.Context(), target, snapshot.InputsSHA256,
				"failed", []byte(`[{"code":"source_metadata_corrupt"}]`)))
		})
	}
}

func checkF10PublicationDirtiesOnlyWhenActiveHeadChanges(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "head.pdf", fakeHash("e83"), 8, "application/pdf")
	require.NoError(t, err)
	copyNode, err := s.CreateFile(t.Context(), s.RootID(), "head-copy.pdf", fakeHash("e83"), 8, "application/pdf")
	require.NoError(t, err)
	canonicalA := mustSourceMetadata(t, "A")
	canonicalB := mustSourceMetadata(t, "B")

	_, err = s.PublishSourceMetadata(t.Context(), node.BlobHash, fakeHash("f83a"), canonicalA)
	require.NoError(t, err)
	require.Equal(t, int64(1), documentEventDirtyRevision(t, s, node.CurrentVersionID))
	require.Equal(t, int64(1), documentEventDirtyRevision(t, s, copyNode.CurrentVersionID))
	version, err := s.ContentVersionByID(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	target := DocumentEventTarget{
		ContentVersionID: node.CurrentVersionID, BlobHash: node.BlobHash, MIMEType: node.MimeType,
		RecordedAt: version.RecordedAt,
		NodeID:     node.ID, Size: node.Size, InputEpoch: 1, InputRevision: 1,
	}
	beforePublicationTime, err := s.LoadDocumentEventEvidence(t.Context(), target)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `UPDATE source_metadata_heads SET published_at=$1 WHERE source_sha256=$2`,
		"2099-01-01T00:00:00Z", node.BlobHash)
	require.NoError(t, err)
	afterPublicationTime, err := s.LoadDocumentEventEvidence(t.Context(), target)
	require.NoError(t, err)
	require.Equal(t, beforePublicationTime.InputsSHA256, afterPublicationTime.InputsSHA256,
		"mutable publication time is not evidence identity")
	_, err = s.PublishSourceMetadata(t.Context(), node.BlobHash, fakeHash("f83a"), canonicalA)
	require.NoError(t, err)
	require.Equal(t, int64(1), documentEventDirtyRevision(t, s, node.CurrentVersionID),
		"same-head publication must not dirty sharing versions")
	require.Equal(t, int64(1), documentEventDirtyRevision(t, s, copyNode.CurrentVersionID))
	_, err = s.PublishSourceMetadata(t.Context(), node.BlobHash, fakeHash("f83b"), canonicalB)
	require.NoError(t, err)
	require.Equal(t, int64(2), documentEventDirtyRevision(t, s, node.CurrentVersionID))
	require.Equal(t, int64(2), documentEventDirtyRevision(t, s, copyNode.CurrentVersionID))
}

func checkDocumentEventPublicationRequiresCapturedManifestDigest(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	version, _ := ingestDocumentEventTarget(t, s, "publish.txt", "e84")
	require.NoError(t, s.EnsureDocumentEventRecipe(t.Context(), DocumentEventsDeriverFingerprint))
	target := requireDocumentEventTarget(t, s, version.ID)
	snapshot, err := s.LoadDocumentEventEvidence(t.Context(), target)
	require.NoError(t, err)
	canonical := mustMarshalDocumentEvents(t,
		documentEventRecord(t, s.VaultID(), version.ID, "e85"))

	_, err = s.PublishDocumentEvents(t.Context(), target, DocumentEventsDeriverFingerprint,
		fakeHash("e86"), canonical)
	require.ErrorIs(t, err, ErrDocumentEventInputsChanged)
	_, err = s.PublishDocumentEvents(t.Context(), target, DocumentEventsDeriverFingerprint,
		snapshot.InputsSHA256, canonical)
	require.NoError(t, err)
}

func mustSourceMetadata(t *testing.T, title string) []byte {
	t.Helper()
	canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{
		ContractVersion: document.SourceMetadataContractV1,
		Fields: []document.SourceMetadataFieldV1{{
			Key: "title", Namespace: "pdf.info", SourceField: "Title",
			Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: &title},
		}},
	})
	require.NoError(t, err)
	return canonical
}

func documentEventDirtyRevision(t *testing.T, s *Store, versionID string) int64 {
	t.Helper()
	var revision int64
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COALESCE((SELECT revision
		FROM document_event_dirty WHERE content_version_id=$1),0)`, versionID).Scan(&revision))
	return revision
}

func documentEventTargetWithBindings(t *testing.T, count int) (*Store, DocumentEventTarget) {
	t.Helper()
	s := newTestStore(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "bounded.pdf", fakeHash("b0bd"), 8, "application/pdf")
	require.NoError(t, err)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		for index := range count {
			ingestID := fmt.Sprintf("10000000-0000-4000-8000-%012x", index+1)
			identity := fakeHash(fmt.Sprintf("%04x", index+1))
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO ingests(id,started_at,source_kind,source_desc)
				VALUES($1,$2,$3,$4)`, ingestID, "2024-01-02T03:04:05Z", "test", "bounded evidence"); err != nil {
				return err
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO provenance(identity,node_id,ingest_id,original_path)
				VALUES($1,$2,$3,$4)`, identity, node.ID, ingestID, fmt.Sprintf("/synthetic/%04d", index)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO provenance_version_bindings(
				provenance_identity,content_version_id,observed_at,basis_ref) VALUES($1,$2,$3,$4)`,
				identity, node.CurrentVersionID, "2024-01-02T03:04:05Z", "test"); err != nil {
				return err
			}
		}
		return nil
	}))
	require.NoError(t, s.EnsureDocumentEventRecipe(t.Context(), DocumentEventsDeriverFingerprint))
	return s, requireDocumentEventTarget(t, s, node.CurrentVersionID)
}
