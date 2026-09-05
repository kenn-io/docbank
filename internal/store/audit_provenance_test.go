package store

import (
	"bytes"
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/audit"
)

func TestAuditedProvenanceAppendAndSupersessionRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	node, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)

	mtime := "2026-08-26T12:00:00Z"
	first, err := s.AppendNodeProvenance(t.Context(), ProvenanceAppendInput{
		NodeID: node.ID, IfRevision: node.Revision, SourceKind: "agent",
		SourceDescription: "triage", OriginalPath: "opaque://report", OriginalMTime: &mtime,
	})
	require.NoError(t, err)
	assert.Equal(t, node.Revision+1, first.Node.Revision)
	assert.Equal(t, "provenance_add", auditEventKindForSequence(t, s, 2))

	second, err := s.AppendNodeProvenance(t.Context(), ProvenanceAppendInput{
		NodeID: node.ID, IfRevision: first.Node.Revision, SourceKind: "agent",
		SourceDescription: "reconcile", OriginalPath: "opaque://report-corrected",
		Supersedes: &first.Fact.Identity,
	})
	require.NoError(t, err)
	assert.Equal(t, first.Node.Revision+1, second.Node.Revision)
	assert.Equal(t, "provenance_supersede", auditEventKindForSequence(t, s, 3))
	page, err := s.NodeProvenance(t.Context(), node.ID, 10, 0)
	require.NoError(t, err)
	require.Len(t, page.Items, 3)
	assert.False(t, findProvenanceFact(page.Items, first.Fact.Identity).Active)
	assert.True(t, findProvenanceFact(page.Items, second.Fact.Identity).Active)
	history, err := s.AuditHistory(t.Context(), node.ID, 10, "")
	require.NoError(t, err)
	var supersession AuditEvent
	for _, event := range history.Items {
		if event.Kind == "provenance_supersede" {
			supersession = event
			break
		}
	}
	require.NotNil(t, supersession.Attachment)
	require.NotNil(t, supersession.Attachment.Before)
	require.NotNil(t, supersession.Attachment.After)
	assert.Equal(t, first.Fact.Identity, supersession.Attachment.Before.ProvenanceID)
	assert.Equal(t, second.Fact.Identity, supersession.Attachment.After.ProvenanceID)
	require.NoError(t, s.ValidateMetadata(t.Context()))

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

func TestAuditedProvenanceAppendRollsBackAllMetadata(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	node, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	require.NoError(t, createAuditScopeFailureTrigger(s))

	_, err = s.AppendNodeProvenance(t.Context(), ProvenanceAppendInput{
		NodeID: node.ID, IfRevision: node.Revision, SourceKind: "agent",
		SourceDescription: "rollback", OriginalPath: "opaque://rollback",
	})
	require.ErrorContains(t, err, "forced audited provenance failure")
	unchanged, err := s.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, node.Revision, unchanged.Revision)
	var ingests, provenance, sequence int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM ingests WHERE source_desc=?),
		(SELECT COUNT(*) FROM provenance WHERE original_path=?),
		(SELECT operation_sequence_high_water FROM audit_authority)`,
		"rollback", "opaque://rollback").Scan(&ingests, &provenance, &sequence))
	assert.Zero(t, ingests)
	assert.Zero(t, provenance)
	assert.Equal(t, int64(1), sequence)
}

func TestAuditedProvenanceReplayRejectsTamperedIngestIdentity(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	node, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	_, err = s.AppendNodeProvenance(t.Context(), ProvenanceAppendInput{
		NodeID: node.ID, IfRevision: node.Revision, SourceKind: "agent",
		SourceDescription: "tamper", OriginalPath: "opaque://source",
	})
	require.NoError(t, err)

	authority, auditScope, err := loadInitialAuditProjection(t.Context(), s.db)
	require.NoError(t, err)
	records, err := loadInitialAuditRecords(t.Context(), s.db)
	require.NoError(t, err)
	initial, err := selectInitialAuditRecords(authority, auditScope, records)
	require.NoError(t, err)
	replay, err := newAuditedHistoryReplay(authority, auditScope, initial)
	require.NoError(t, err)
	mutations, err := auditRecordsByOptionalSequence(records["canonical_mutation"], authority.sequence)
	require.NoError(t, err)
	mutation := mutations[2]
	deltas := auditRecordsByDigest(records["attached_metadata_delta"])
	digest, err := auditDigestField(mutation.record, "attached_metadata_change_digest")
	require.NoError(t, err)
	delta := deltas[digest]
	changes, err := auditRecordListField(delta.record, "changes")
	require.NoError(t, err)
	var appendedIngest audit.Record
	for _, change := range changes {
		kind, kindErr := auditTextField(change, "record_kind")
		require.NoError(t, kindErr)
		if kind == metadataIngestType {
			appendedIngest, err = validateAuditedIngestAddition(change)
			require.NoError(t, err)
		}
	}
	ingestKey, err := attachedAuditKey(appendedIngest)
	require.NoError(t, err)
	replay.attachments[ingestKey] = appendedIngest
	used := map[string]bool{}
	operationID, err := auditUUIDField(mutation.record, auditOperationIDField)
	require.NoError(t, err)
	_, err = replay.validateProvenanceAppendDelta(mutation.record, operationID, deltas, used)
	require.ErrorContains(t, err, "provenance mutation reuses ingest identity")
	delete(replay.attachments, ingestKey)
	for index, change := range changes {
		kind, kindErr := auditTextField(change, "record_kind")
		require.NoError(t, kindErr)
		if kind != metadataIngestType {
			continue
		}
		tampered, tamperErr := replaceAuditRecordField(change, "stable_identity", audit.Nested(audit.Record{
			Kind: "tampered_identity",
		}))
		require.NoError(t, tamperErr)
		changes[index] = tampered
	}
	delta.record, err = replaceAuditRecordField(delta.record, "changes", audit.List(auditNestedValues(changes)...))
	require.NoError(t, err)
	deltas[digest] = delta
	used = map[string]bool{}
	_, err = replay.validateProvenanceAppendDelta(mutation.record, operationID, deltas, used)
	require.ErrorContains(t, err, "audited ingest attachment identity does not match its post record")
}

func TestAuditedProvenanceImportRejectsTamperedFact(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	node, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	_, err = s.AppendNodeProvenance(t.Context(), ProvenanceAppendInput{
		NodeID: node.ID, IfRevision: node.Revision, SourceKind: "agent",
		SourceDescription: "tamper", OriginalPath: "opaque://original",
	})
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	malformed := rewriteAppendedProvenancePath(t, exported.Bytes(), node.ID)

	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	err = restored.ImportMetadata(t.Context(), bytes.NewReader(malformed))
	require.ErrorContains(t, err, "replayed audit attachments do not match current metadata")
	var auditRows int64
	require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM audit_records`).Scan(&auditRows))
	assert.Zero(t, auditRows)
}

func findProvenanceFact(facts []ProvenanceFact, identity string) ProvenanceFact {
	for _, fact := range facts {
		if fact.Identity == identity {
			return fact
		}
	}
	return ProvenanceFact{}
}

func createAuditScopeFailureTrigger(s *Store) error {
	_, err := s.db.Exec(`CREATE TRIGGER reject_provenance_scope_advance
		BEFORE UPDATE ON audit_scopes BEGIN
		SELECT RAISE(ABORT, 'forced audited provenance failure'); END`)
	return err
}

func rewriteAppendedProvenancePath(t *testing.T, input []byte, nodeID int64) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	found := false
	for index, line := range lines {
		var header struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &header))
		if header.Type != metadataProvenanceType {
			continue
		}
		var provenance metadataProvenance
		require.NoError(t, json.Unmarshal(line, &provenance))
		if provenance.NodeID == nodeID && provenance.OriginalPath == "opaque://original" {
			provenance.OriginalPath = "opaque://tampered"
			var err error
			provenance.Identity, err = provenanceIdentity(provenance)
			require.NoError(t, err)
			lines[index], err = json.Marshal(provenance)
			require.NoError(t, err)
			found = true
			break
		}
	}
	require.True(t, found)
	return append(bytes.Join(lines, []byte{'\n'}), '\n')
}
