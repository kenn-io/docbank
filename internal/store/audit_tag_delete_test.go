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

func TestAuditedTagDeleteRoundTripsCompleteFanout(t *testing.T) {
	s, tag, report := newAuditedTagStore(t)
	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	reportAssignment, err := s.AssignTag(
		t.Context(), tag.ID, report.ID, report.Revision,
	)
	require.NoError(t, err)
	projectsAssignment, err := s.AssignTag(
		t.Context(), tag.ID, projects.ID, projects.Revision,
	)
	require.NoError(t, err)

	deleted, err := s.DeleteTag(t.Context(), tag.ID, projectsAssignment.Tag.Revision)
	require.NoError(t, err)
	assert.Equal(t, 2, deleted.AssignmentCount)
	_, err = s.TagByID(t.Context(), tag.ID)
	require.ErrorIs(t, err, ErrNotFound)
	currentReport, err := s.NodeByID(t.Context(), report.ID)
	require.NoError(t, err)
	assert.Equal(t, reportAssignment.Node.Revision+1, currentReport.Revision)
	currentProjects, err := s.NodeByID(t.Context(), projects.ID)
	require.NoError(t, err)
	assert.Equal(t, projectsAssignment.Node.Revision+1, currentProjects.Revision)
	events := auditEventsForSequence(t, s, 4)
	require.Len(t, events, 4)
	assert.Equal(t, []string{
		"tag_delete", "tag_unassign", "tag_delete", "tag_unassign",
	}, auditEventKinds(t, events))
	assert.Equal(t, []uint64{
		uint64(projects.ID), uint64(projects.ID), uint64(report.ID), uint64(report.ID),
	}, auditEventNodeIDs(t, events))
	require.NoError(t, s.ValidateMetadata(t.Context()))
	assertAuditMetadataRoundTrip(t, s)
}

func TestAuditedTagDeleteOutsideScopeAdvancesOnlyAllocation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	tag, err := s.CreateTag(t.Context(), "reviewed")
	require.NoError(t, err)
	empty, err := s.NodeByPath(t.Context(), "/Empty")
	require.NoError(t, err)
	assigned, err := s.AssignTag(t.Context(), tag.ID, empty.ID, empty.Revision)
	require.NoError(t, err)
	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, projects.ID)

	_, err = s.DeleteTag(t.Context(), tag.ID, assigned.Tag.Revision)
	require.NoError(t, err)
	current, err := s.NodeByID(t.Context(), empty.ID)
	require.NoError(t, err)
	assert.Equal(t, assigned.Node.Revision+1, current.Revision)
	assert.Equal(t, assigned.Node.ModifiedAt, current.ModifiedAt)
	var sequence, scopeEntries, mutations int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT operation_sequence_high_water FROM audit_authority),
		(SELECT entry_count FROM audit_scopes),
		(SELECT COUNT(*) FROM audit_records WHERE kind='canonical_mutation')`,
	).Scan(&sequence, &scopeEntries, &mutations))
	assert.Equal(t, int64(2), sequence)
	assert.Equal(t, int64(1), scopeEntries)
	assert.Equal(t, int64(1), mutations)
	records, err := loadInitialAuditRecords(t.Context(), s.db)
	require.NoError(t, err)
	allocations, err := auditRecordsBySequence(records["allocation_entry"], sequence)
	require.NoError(t, err)
	changes, err := auditUnsignedField(
		allocations[2].record, auditAttachedMetadataChangeCountField,
	)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), changes)
	require.NoError(t, s.ValidateMetadata(t.Context()))
	assertAuditMetadataRoundTrip(t, s)
}

func TestAuditedTagDeleteRollsBackDefinitionNodesAndHistory(t *testing.T) {
	s, tag, report := newAuditedTagStore(t)
	assigned, err := s.AssignTag(t.Context(), tag.ID, report.ID, report.Revision)
	require.NoError(t, err)
	_, err = s.db.Exec(`CREATE TRIGGER reject_audit_tag_delete_scope_advance
		BEFORE UPDATE ON audit_scopes BEGIN
		SELECT RAISE(ABORT, 'forced audit tag delete failure'); END`)
	require.NoError(t, err)

	_, err = s.DeleteTag(t.Context(), tag.ID, assigned.Tag.Revision)
	require.ErrorContains(t, err, "forced audit tag delete failure")
	currentTag, err := s.TagByID(t.Context(), tag.ID)
	require.NoError(t, err)
	assert.Equal(t, assigned.Tag, currentTag)
	currentNode, err := s.NodeByID(t.Context(), report.ID)
	require.NoError(t, err)
	assert.Equal(t, assigned.Node.Revision, currentNode.Revision)
	var sequence int64
	require.NoError(t, s.db.QueryRow(
		`SELECT operation_sequence_high_water FROM audit_authority`,
	).Scan(&sequence))
	assert.Equal(t, int64(2), sequence)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestAuditedTagDeleteImportRejectsReappearingDefinition(t *testing.T) {
	s, tag, _ := newAuditedTagStore(t)
	_, err := s.DeleteTag(t.Context(), tag.ID, tag.Revision)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	malformed := appendMetadataTag(t, exported.Bytes(), tag)

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

func TestAuditedTagDeleteReplayRejectsOmittedAssignmentTombstone(t *testing.T) {
	s, tag, report := newAuditedTagStore(t)
	assigned, err := s.AssignTag(t.Context(), tag.ID, report.ID, report.Revision)
	require.NoError(t, err)
	_, err = s.DeleteTag(t.Context(), tag.ID, assigned.Tag.Revision)
	require.NoError(t, err)
	replay, records, authority, scope := loadAuditReplayFixture(t, s)
	mutations, err := auditRecordsByOptionalSequence(records["canonical_mutation"], authority.sequence)
	require.NoError(t, err)
	allocations, err := auditRecordsBySequence(records["allocation_entry"], authority.sequence)
	require.NoError(t, err)
	entries, err := auditScopeRecordsByCount(records["scope_chain_entry"], scope)
	require.NoError(t, err)
	deltas := auditRecordsByDigest(records["attached_metadata_delta"])
	events, err := auditEventRecordsByID(records[auditEventField])
	require.NoError(t, err)
	usedDeltas, usedEvents := map[string]bool{}, map[string]bool{}
	require.NoError(t, replay.applyTagAssignment(
		s.vaultID, mutations[2], allocations[2], entries[2],
		deltas, events, usedDeltas, usedEvents,
	))
	deleteDigest, err := auditDigestField(
		mutations[3].record, "attached_metadata_change_digest",
	)
	require.NoError(t, err)
	malformed := deltas[deleteDigest]
	changes, err := auditRecordListField(malformed.record, "changes")
	require.NoError(t, err)
	filtered := changes[:0]
	for _, change := range changes {
		kind, err := auditTextField(change, "record_kind")
		require.NoError(t, err)
		if kind != "tag_assignment" {
			filtered = append(filtered, change)
		}
	}
	malformed.record = mustReplaceAuditRecordField(
		t, malformed.record, "changes", audit.List(auditNestedValues(filtered)...),
	)
	deltas[deleteDigest] = malformed

	err = replay.applyTagDefinitionDelete(
		s.vaultID, mutations[3], allocations[3], []string{scope.scopeID},
		map[string]storedAuditRecord{scope.scopeID: entries[3]},
		deltas, events, usedDeltas, usedEvents,
	)
	require.ErrorContains(t, err, "complete assignment set")
}

func TestAuditedTagDeleteReplayRetainsUsedDefinitionIdentity(t *testing.T) {
	s, tag, _ := newAuditedTagStore(t)
	_, err := s.DeleteTag(t.Context(), tag.ID, tag.Revision)
	require.NoError(t, err)
	replay, records, authority, _ := loadAuditReplayFixture(t, s)
	allocations, err := auditRecordsBySequence(records["allocation_entry"], authority.sequence)
	require.NoError(t, err)
	deltas := auditRecordsByDigest(records["attached_metadata_delta"])
	require.NoError(t, replay.applyUnscopedTagDefinitionChange(
		s.vaultID, allocations[2], deltas, map[string]bool{},
	))
	reused := tag
	reused.Name = "reused"
	definition, err := tagDefinitionAuditRecord(reused)
	require.NoError(t, err)
	change, err := makeAttachedMetadataPresenceChange(definition, true)
	require.NoError(t, err)
	operationID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	operationValue, err := audit.UUID(operationID)
	require.NoError(t, err)
	delta, digest, err := makeAttachedMetadataDelta(operationValue, []audit.Record{change})
	require.NoError(t, err)

	_, err = replay.validateTagDefinitionDelta(
		operationID, digest.text,
		map[string]storedAuditRecord{digest.text: {digest: digest.text, record: delta}},
		map[string]bool{},
	)
	require.ErrorContains(t, err, "reuses definition identity")
}

func loadAuditReplayFixture(
	t *testing.T, s *Store,
) (*auditedHistoryReplay, map[string][]storedAuditRecord, initialAuditAuthority, initialAuditScope) {
	t.Helper()
	authority, scope, err := loadInitialAuditProjection(t.Context(), s.db)
	require.NoError(t, err)
	records, err := loadInitialAuditRecords(t.Context(), s.db)
	require.NoError(t, err)
	initial, err := selectInitialAuditRecords(authority, scope, records)
	require.NoError(t, err)
	replay, err := newAuditedHistoryReplay(authority, scope, initial)
	require.NoError(t, err)
	return replay, records, authority, scope
}

func auditEventsForSequence(t *testing.T, s *Store, sequence int64) []audit.Record {
	t.Helper()
	records, err := loadInitialAuditRecords(t.Context(), s.db)
	require.NoError(t, err)
	mutations, err := auditRecordsByOptionalSequence(records["canonical_mutation"], sequence)
	require.NoError(t, err)
	events, err := auditRecordListField(mutations[sequence].record, "events")
	require.NoError(t, err)
	return events
}

func auditEventKinds(t *testing.T, events []audit.Record) []string {
	t.Helper()
	result := make([]string, len(events))
	for index, event := range events {
		var err error
		result[index], err = auditTextField(event, "event_kind")
		require.NoError(t, err)
	}
	return result
}

func auditEventNodeIDs(t *testing.T, events []audit.Record) []uint64 {
	t.Helper()
	result := make([]uint64, len(events))
	for index, event := range events {
		var err error
		result[index], err = auditUnsignedField(event, metadataNodeIDField)
		require.NoError(t, err)
	}
	return result
}

func appendMetadataTag(t *testing.T, input []byte, tag Tag) []byte {
	t.Helper()
	encoded, err := json.Marshal(metadataTag{
		Type: metadataTagRecordType, ID: tag.ID, Name: tag.Name, Revision: tag.Revision,
	})
	require.NoError(t, err)
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	for index, line := range lines {
		var header struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &header))
		if header.Type != metadataAuditAuthorityType {
			continue
		}
		lines = append(lines, nil)
		copy(lines[index+1:], lines[index:])
		lines[index] = encoded
		return append(bytes.Join(lines, []byte{'\n'}), '\n')
	}
	t.Fatal("metadata export lacks audit authority")
	return nil
}
