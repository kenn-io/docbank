package store

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/audit"
)

func TestAuditedIngestRecordsProvenanceAndRoundTrips(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)

	run, err := s.BeginIngest(t.Context(), "cli", "/source/reports")
	require.NoError(t, err)
	var runRows int64
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM ingests WHERE id=?`, run.ID(),
	).Scan(&runRows))
	assert.Zero(t, runRows, "preparing a run must not publish metadata authority")

	first, added, err := s.IngestFile(
		t.Context(), run, scope.ID, "first.txt", fakeHash("ad1"), 11,
		"text/plain", "/source/reports/first.txt", "2026-07-17T12:00:00Z",
	)
	require.NoError(t, err)
	require.True(t, added)
	second, added, err := s.IngestFile(
		t.Context(), run, scope.ID, "second.txt", fakeHash("ad2"), 12,
		"text/plain", "/source/reports/second.txt", "2026-07-17T12:00:01Z",
	)
	require.NoError(t, err)
	require.True(t, added)

	skipped, added, err := s.IngestFile(
		t.Context(), run, scope.ID, "second.txt", fakeHash("ad2"), 12,
		"text/plain", "/source/reports/second.txt", "2026-07-17T12:00:01Z",
	)
	require.NoError(t, err)
	assert.False(t, added)
	assert.Equal(t, second.ID, skipped.ID)
	require.NoError(t, s.ValidateMetadata(t.Context()))

	var ingests, provenance, sequence, membership int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM ingests WHERE id=?),
		(SELECT COUNT(*) FROM provenance WHERE ingest_id=?),
		(SELECT operation_sequence_high_water FROM audit_authority),
		(SELECT COUNT(*) FROM audit_memberships WHERE node_id IN (?,?))`,
		run.ID(), run.ID(), first.ID, second.ID,
	).Scan(&ingests, &provenance, &sequence, &membership))
	assert.Equal(t, int64(1), ingests)
	assert.Equal(t, int64(2), provenance)
	assert.Equal(t, int64(3), sequence)
	assert.Equal(t, int64(2), membership)

	rows, err := s.db.Query(`SELECT record_json FROM audit_records
		WHERE kind='canonical_mutation' AND operation_sequence>1 ORDER BY operation_sequence`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var attachmentCounts []uint64
	for rows.Next() {
		var raw []byte
		require.NoError(t, rows.Scan(&raw))
		record, err := audit.UnmarshalJSONRecord(raw)
		require.NoError(t, err)
		groupingID, err := auditUUIDField(record, "grouping_id")
		require.NoError(t, err)
		assert.Equal(t, run.ID(), groupingID)
		count, err := auditUnsignedField(record, auditAttachedMetadataChangeCountField)
		require.NoError(t, err)
		attachmentCounts = append(attachmentCounts, count)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []uint64{2, 1}, attachmentCounts)

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	var roundTrip bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &roundTrip))
	assert.Equal(t, exported.Bytes(), roundTrip.Bytes())
}

func TestAuditedIngestRollsBackFileAndAttachments(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	run, err := s.BeginIngest(t.Context(), "cli", "/source")
	require.NoError(t, err)
	_, err = s.db.Exec(`CREATE TRIGGER reject_ingest_scope_advance
		BEFORE UPDATE ON audit_scopes BEGIN
		SELECT RAISE(ABORT, 'forced audited ingest failure'); END`)
	require.NoError(t, err)

	_, _, err = s.IngestFile(
		t.Context(), run, scope.ID, "rollback.txt", fakeHash("ad3"), 7,
		"text/plain", "/source/rollback.txt", "",
	)
	require.ErrorContains(t, err, "forced audited ingest failure")
	_, err = s.NodeByPath(t.Context(), "/Projects/rollback.txt")
	require.ErrorIs(t, err, ErrNotFound)
	var ingestRows, provenanceRows int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM ingests WHERE id=?),
		(SELECT COUNT(*) FROM provenance WHERE ingest_id=?)`,
		run.ID(), run.ID(),
	).Scan(&ingestRows, &provenanceRows))
	assert.Zero(t, ingestRows)
	assert.Zero(t, provenanceRows)
}

func TestAuditedIngestImportRejectsOmittedProvenance(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	run, err := s.BeginIngest(t.Context(), "cli", "/source")
	require.NoError(t, err)
	created, added, err := s.IngestFile(
		t.Context(), run, scope.ID, "missing.txt", fakeHash("ad4"), 9,
		"text/plain", "/source/missing.txt", "",
	)
	require.NoError(t, err)
	require.True(t, added)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	malformed := omitMetadataProvenance(t, exported.Bytes(), created.ID)

	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	err = restored.ImportMetadata(t.Context(), bytes.NewReader(malformed))
	require.ErrorContains(t, err, "replayed audit attachments do not match current metadata")
	var auditRows int64
	require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM audit_records`).Scan(&auditRows))
	assert.Zero(t, auditRows)
}

func TestAuditReplayRejectsGenesisProvenanceFromLaterIngest(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scopeNode, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scopeNode.ID)
	run, err := s.BeginIngest(t.Context(), "cli", "/later")
	require.NoError(t, err)
	_, added, err := s.IngestFile(
		t.Context(), run, scopeNode.ID, "later.txt", fakeHash("ad5"), 10,
		"text/plain", "/later/later.txt", "",
	)
	require.NoError(t, err)
	require.True(t, added)

	authority, scope, err := loadInitialAuditProjection(t.Context(), s.db)
	require.NoError(t, err)
	records, err := loadInitialAuditRecords(t.Context(), s.db)
	require.NoError(t, err)
	initial, err := selectInitialAuditRecords(authority, scope, records)
	require.NoError(t, err)
	deltaChanges, err := auditRecordListField(records["attached_metadata_delta"][0].record, "changes")
	require.NoError(t, err)
	var laterProvenance audit.Record
	for _, change := range deltaChanges {
		post, postErr := auditNestedField(change, "post")
		require.NoError(t, postErr)
		if post.Kind == metadataProvenanceType {
			laterProvenance = post
		}
	}
	require.Equal(t, metadataProvenanceType, laterProvenance.Kind)

	genesis := initial["attached_metadata_genesis"][0]
	genesisAttachments, err := auditRecordListField(genesis.record, "records")
	require.NoError(t, err)
	genesisAttachments = append(genesisAttachments, laterProvenance)
	genesis.record, err = replaceAuditRecordField(
		genesis.record, "records", audit.List(auditNestedValues(genesisAttachments)...),
	)
	require.NoError(t, err)
	initial["attached_metadata_genesis"][0] = genesis

	_, err = newAuditedHistoryReplay(authority, scope, initial)
	require.ErrorContains(t, err, "genesis provenance references missing ingest "+run.ID())
}

func omitMetadataProvenance(t *testing.T, input []byte, nodeID int64) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	kept := lines[:0]
	found := false
	for _, line := range lines {
		var header struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &header))
		if header.Type == metadataProvenanceType {
			var provenance metadataProvenance
			require.NoError(t, json.Unmarshal(line, &provenance))
			if provenance.NodeID == nodeID {
				found = true
				continue
			}
		}
		kept = append(kept, line)
	}
	require.True(t, found)
	return append(bytes.Join(kept, []byte{'\n'}), '\n')
}
