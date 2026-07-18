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

func TestAuditedTagCreationRoundTripsBeforeAssignment(t *testing.T) {
	s, _, report := newAuditedTagStore(t)
	created, err := s.CreateTag(t.Context(), "post-enrollment")
	require.NoError(t, err)
	assert.Equal(t, int64(1), created.Revision)
	assert.Zero(t, created.AssignmentCount)

	assigned, err := s.AssignTag(
		t.Context(), created.ID, report.ID, report.Revision,
	)
	require.NoError(t, err)
	assert.True(t, assigned.Changed)
	require.NoError(t, s.ValidateMetadata(t.Context()))
	assert.Equal(t, "tag_assign", auditEventKindForSequence(t, s, 3))

	var sequence, allocations, scopeEntries, mutations, deltas int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT operation_sequence_high_water FROM audit_authority),
		(SELECT allocation_entry_count FROM audit_authority),
		(SELECT entry_count FROM audit_scopes),
		(SELECT COUNT(*) FROM audit_records WHERE kind='canonical_mutation'),
		(SELECT COUNT(*) FROM audit_records WHERE kind='attached_metadata_delta')`,
	).Scan(&sequence, &allocations, &scopeEntries, &mutations, &deltas))
	assert.Equal(t, int64(3), sequence)
	assert.Equal(t, int64(3), allocations)
	assert.Equal(t, int64(2), scopeEntries)
	assert.Equal(t, int64(2), mutations)
	assert.Equal(t, int64(2), deltas)

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

func TestAuditedTagCreationNameConflictDoesNotAdvanceLineage(t *testing.T) {
	s, existing, _ := newAuditedTagStore(t)
	_, err := s.CreateTag(t.Context(), existing.Name)
	require.ErrorIs(t, err, ErrExists)

	var sequence, deltas int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT operation_sequence_high_water FROM audit_authority),
		(SELECT COUNT(*) FROM audit_records WHERE kind='attached_metadata_delta')`,
	).Scan(&sequence, &deltas))
	assert.Equal(t, int64(1), sequence)
	assert.Zero(t, deltas)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestAuditedTagCreationRollsBackWithLineage(t *testing.T) {
	s, _, _ := newAuditedTagStore(t)
	_, err := s.db.Exec(`CREATE TRIGGER reject_tag_creation_authority_advance
		BEFORE UPDATE ON audit_authority BEGIN
		SELECT RAISE(ABORT, 'forced tag-creation failure'); END`)
	require.NoError(t, err)

	_, err = s.CreateTag(t.Context(), "rollback")
	require.ErrorContains(t, err, "forced tag-creation failure")
	_, err = s.TagByName(t.Context(), "rollback")
	require.ErrorIs(t, err, ErrNotFound)
	var sequence, deltas int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT operation_sequence_high_water FROM audit_authority),
		(SELECT COUNT(*) FROM audit_records WHERE kind='attached_metadata_delta')`,
	).Scan(&sequence, &deltas))
	assert.Equal(t, int64(1), sequence)
	assert.Zero(t, deltas)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestAuditedTagCreationImportRejectsMissingCurrentDefinition(t *testing.T) {
	s, _, _ := newAuditedTagStore(t)
	created, err := s.CreateTag(t.Context(), "missing")
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	malformed := omitMetadataTagDefinition(t, exported.Bytes(), created.ID)

	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	err = restored.ImportMetadata(t.Context(), bytes.NewReader(malformed))
	require.ErrorContains(t, err, "replayed audit attachments do not match current metadata")
	var tagRows, auditRows int64
	require.NoError(t, restored.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM tags),
		(SELECT COUNT(*) FROM audit_records)`).Scan(&tagRows, &auditRows))
	assert.Zero(t, tagRows)
	assert.Zero(t, auditRows)
}

func TestAuditedTagCreationReplayRejectsReusedIdentity(t *testing.T) {
	s, existing, _ := newAuditedTagStore(t)
	_, err := s.CreateTag(t.Context(), "new")
	require.NoError(t, err)
	authority, scope, err := loadInitialAuditProjection(t.Context(), s.db)
	require.NoError(t, err)
	records, err := loadInitialAuditRecords(t.Context(), s.db)
	require.NoError(t, err)
	initial, err := selectInitialAuditRecords(authority, scope, records)
	require.NoError(t, err)
	replay, err := newAuditedHistoryReplay(authority, scope, initial)
	require.NoError(t, err)
	allocations, err := auditRecordsBySequence(records["allocation_entry"], authority.sequence)
	require.NoError(t, err)
	allocation := allocations[2]
	digest, err := auditDigestField(allocation.record, "attached_metadata_change_digest")
	require.NoError(t, err)
	deltas := auditRecordsByDigest(records["attached_metadata_delta"])
	delta := deltas[digest]
	existingDefinition, err := tagDefinitionAuditRecord(existing)
	require.NoError(t, err)
	change, err := makeAttachedMetadataPresenceChange(existingDefinition, true)
	require.NoError(t, err)
	delta.record = mustReplaceAuditRecordField(
		t, delta.record, "changes", audit.List(audit.Nested(change)),
	)
	deltas[digest] = delta

	_, err = replay.validateTagCreationDelta(
		mustAuditOperationID(t, allocation.record),
		digest, deltas, map[string]bool{},
	)
	require.ErrorContains(t, err, "reuses definition identity")
}

func omitMetadataTagDefinition(t *testing.T, input []byte, tagID string) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	kept := lines[:0]
	found := false
	for _, line := range lines {
		var header struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &header))
		if header.Type == "tag" {
			var definition metadataTag
			require.NoError(t, json.Unmarshal(line, &definition))
			if definition.ID == tagID {
				found = true
				continue
			}
		}
		kept = append(kept, line)
	}
	require.True(t, found)
	return append(bytes.Join(kept, []byte{'\n'}), '\n')
}
