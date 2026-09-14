package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/audit"
)

func TestLegacyAuditedIngestCreationRoundTripsWithoutBindings(t *testing.T) {
	s := newTestStore(t)
	seedMetadataRoundTrip(t, s)
	s.vaultID = "99999999-9999-4999-8999-999999999999"
	_, err := s.db.Exec(`UPDATE vault_metadata SET vault_uid=?`, s.vaultID)
	require.NoError(t, err)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)

	run := IngestRun{record: metadataIngest{
		Type: metadataIngestType, ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		StartedAt: testAuditTimestamp, SourceKind: "cli", SourceDesc: "Synthetic legacy ingest",
	}}
	operation := contentVersionOperation{
		versionID:   "dddddddd-dddd-4ddd-8ddd-dddddddddddd",
		operationID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		recordedAt:  testAuditTimestamp,
	}
	var created Node
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		prior, err := liveDirTx(tx, scope.ID)
		if err != nil {
			return err
		}
		authority, scopes, _, err := loadAuditedNodeAuthority(t.Context(), tx, scope.ID)
		if err != nil {
			return err
		}
		ingestAdded, err := ensureIngestRunTx(t.Context(), tx, run)
		if err != nil {
			return err
		}
		var version ContentVersion
		created, version, err = s.createFileWithOperationTx(
			t.Context(), tx, scope.ID, "legacy-ingest.txt", fakeHash("ad0"), 13,
			"text/plain", operation,
		)
		if err != nil {
			return err
		}
		originalMtime := "2026-07-16T08:00:00Z"
		provenance := metadataProvenance{
			Type: metadataProvenanceType, NodeID: created.ID, IngestID: run.ID(),
			OriginalPath: "/synthetic/legacy-ingest.txt", OriginalMTime: &originalMtime,
		}
		provenance.Identity, err = provenanceIdentity(provenance)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO provenance(
			identity,node_id,ingest_id,original_path,original_mtime,supersedes
		) VALUES(?,?,?,?,?,?)`, provenance.Identity, provenance.NodeID, provenance.IngestID,
			provenance.OriginalPath, provenance.OriginalMTime, provenance.Supersedes); err != nil {
			return err
		}
		resulting, err := nodeByIDTx(tx, scope.ID)
		if err != nil {
			return err
		}
		legacy, err := makeLegacyAuditedIngestCreationMetadata(
			run.record, provenance, ingestAdded, operation.operationID,
		)
		if err != nil {
			return err
		}
		return persistAuditedNodeCreation(
			t.Context(), tx, s.vaultID, authority, scopes, prior, resulting,
			created, version, operation.operationID, operation.recordedAt, &legacy,
		)
	}))
	require.NoError(t, s.ValidateMetadata(t.Context()))

	var bindings int64
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM provenance_version_bindings`).Scan(&bindings))
	assert.Zero(t, bindings)
	records, err := loadInitialAuditRecords(t.Context(), s.db)
	require.NoError(t, err)
	mutations, err := auditRecordsByOptionalSequence(records["canonical_mutation"], 2)
	require.NoError(t, err)
	attachmentCount, err := auditUnsignedField(
		mutations[2].record, auditAttachedMetadataChangeCountField,
	)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), attachmentCount,
		"pre-binding creation history carries only ingest and provenance attachments")

	var exported, auditBefore bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	require.NoError(t, exportAuditRecords(
		t.Context(), s.db, newMetadataJSONWriter(&auditBefore),
	))
	assert.Equal(t, "32555732f581b4973572fc1186f8368c2c1b2c3779175513e87f621bed2dc40a",
		fmt.Sprintf("%x", sha256.Sum256(auditBefore.Bytes())))

	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	require.NoError(t, restored.db.QueryRow(
		`SELECT COUNT(*) FROM provenance_version_bindings`,
	).Scan(&bindings))
	assert.Zero(t, bindings)
	var roundTrip, auditAfter bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &roundTrip))
	assert.Equal(t, exported.Bytes(), roundTrip.Bytes())
	require.NoError(t, exportAuditRecords(
		t.Context(), restored.db, newMetadataJSONWriter(&auditAfter),
	))
	assert.Equal(t, auditBefore.Bytes(), auditAfter.Bytes(), "legacy audit hashes must survive import")
	restoredCreated, err := restored.NodeByPath(t.Context(), "/Projects/legacy-ingest.txt")
	require.NoError(t, err)
	assert.Equal(t, created.ID, restoredCreated.ID)
}

func makeLegacyAuditedIngestCreationMetadata(
	ingest metadataIngest, provenance metadataProvenance, ingestAdded bool, operationID string,
) (auditedCreationMetadata, error) {
	ingestRecord, err := ingestAuditRecord(ingest)
	if err != nil {
		return auditedCreationMetadata{}, err
	}
	provenanceRecord, err := provenanceAuditRecord(
		provenance.Identity, provenance.NodeID, provenance.IngestID,
		provenance.OriginalPath, nullString(provenance.OriginalMTime), nullString(provenance.Supersedes),
	)
	if err != nil {
		return auditedCreationMetadata{}, err
	}
	baseline := []audit.Record{ingestRecord, provenanceRecord}
	if err := sortAuditRecordsByCanonicalIdentity(baseline, attachedAuditIdentity); err != nil {
		return auditedCreationMetadata{}, err
	}
	changes := make([]audit.Record, 0, 2)
	if ingestAdded {
		change, err := makeAttachedMetadataAddition(ingestRecord)
		if err != nil {
			return auditedCreationMetadata{}, err
		}
		changes = append(changes, change)
	}
	provenanceChange, err := makeAttachedMetadataAddition(provenanceRecord)
	if err != nil {
		return auditedCreationMetadata{}, err
	}
	changes = append(changes, provenanceChange)
	operationValue, err := audit.UUID(operationID)
	if err != nil {
		return auditedCreationMetadata{}, err
	}
	delta, digest, err := makeAttachedMetadataDelta(operationValue, changes)
	if err != nil {
		return auditedCreationMetadata{}, err
	}
	groupingID, err := audit.UUID(ingest.ID)
	if err != nil {
		return auditedCreationMetadata{}, err
	}
	return auditedCreationMetadata{
		groupingID: groupingID, baselineRecords: baseline, changes: changes,
		delta: delta, deltaDigest: digest, provenance: provenanceRecord,
	}, nil
}

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
	assert.Equal(t, []uint64{3, 2}, attachmentCounts)
	for _, node := range []Node{first, second} {
		bindings, err := s.ProvenanceVersionBindings(t.Context(), node.CurrentVersionID)
		require.NoError(t, err)
		require.Len(t, bindings, 1)
		assert.Equal(t, run.record.StartedAt, bindings[0].ObservedAt)
		assert.Equal(t, provenanceVersionBindingBasis, bindings[0].BasisRef)
	}

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	var roundTrip bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &roundTrip))
	assert.Equal(t, exported.Bytes(), roundTrip.Bytes())
	assert.Contains(t, exported.String(), `"type":"provenance_version_binding"`)
	restoredBindings, err := restored.ProvenanceVersionBindings(t.Context(), first.CurrentVersionID)
	require.NoError(t, err)
	assert.Len(t, restoredBindings, 1)
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
	var dirtyRowsBefore int64
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM document_event_dirty`).Scan(&dirtyRowsBefore))
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
	var ingestRows, provenanceRows, bindingRows, dirtyRows, orphanDirtyRows int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM ingests WHERE id=?),
		(SELECT COUNT(*) FROM provenance WHERE ingest_id=?),
		(SELECT COUNT(*) FROM provenance_version_bindings b
		 JOIN provenance p ON p.identity=b.provenance_identity WHERE p.ingest_id=?),
		(SELECT COUNT(*) FROM document_event_dirty),
		(SELECT COUNT(*) FROM document_event_dirty d LEFT JOIN content_versions cv
		 ON cv.version_id=d.content_version_id WHERE cv.version_id IS NULL)`,
		run.ID(), run.ID(), run.ID(),
	).Scan(&ingestRows, &provenanceRows, &bindingRows, &dirtyRows, &orphanDirtyRows))
	assert.Zero(t, ingestRows)
	assert.Zero(t, provenanceRows)
	assert.Zero(t, bindingRows)
	assert.Equal(t, dirtyRowsBefore, dirtyRows,
		"a rejected creation must roll back the new version's dirty row")
	assert.Zero(t, orphanDirtyRows)
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
