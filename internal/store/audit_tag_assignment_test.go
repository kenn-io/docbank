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

func TestAuditedTagAssignmentRoundTrips(t *testing.T) {
	s, tag, report := newAuditedTagStore(t)
	assigned, err := s.AssignTagPath(t.Context(), tag.ID, "/Projects/report.txt")
	require.NoError(t, err)
	assert.True(t, assigned.Changed)
	assert.Equal(t, report.Revision+1, assigned.Node.Revision)
	assert.Equal(t, tag.Revision+1, assigned.Tag.Revision)
	assert.Equal(t, 1, assigned.Tag.AssignmentCount)
	require.NoError(t, s.ValidateMetadata(t.Context()))
	assert.Equal(t, "tag_assign", auditEventKindForSequence(t, s, 2))

	unassigned, err := s.UnassignTag(
		t.Context(), tag.ID, assigned.Node.ID, assigned.Node.Revision,
	)
	require.NoError(t, err)
	assert.True(t, unassigned.Changed)
	assert.Equal(t, assigned.Node.Revision+1, unassigned.Node.Revision)
	assert.Equal(t, assigned.Tag.Revision+1, unassigned.Tag.Revision)
	assert.Zero(t, unassigned.Tag.AssignmentCount)
	require.NoError(t, s.ValidateMetadata(t.Context()))
	assert.Equal(t, "tag_unassign", auditEventKindForSequence(t, s, 3))

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

func TestAuditedTagAssignmentNoOpDoesNotAdvanceHistory(t *testing.T) {
	s, tag, report := newAuditedTagStore(t)
	assigned, err := s.AssignTag(t.Context(), tag.ID, report.ID, report.Revision)
	require.NoError(t, err)
	var before int64
	require.NoError(t, s.db.QueryRow(
		`SELECT operation_sequence_high_water FROM audit_authority`,
	).Scan(&before))

	repeated, err := s.AssignTag(
		t.Context(), tag.ID, report.ID, assigned.Node.Revision,
	)
	require.NoError(t, err)
	assert.False(t, repeated.Changed)
	assert.Equal(t, assigned.Node.Revision, repeated.Node.Revision)
	var after int64
	require.NoError(t, s.db.QueryRow(
		`SELECT operation_sequence_high_water FROM audit_authority`,
	).Scan(&after))
	assert.Equal(t, before, after)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestAuditedTagAssignmentOutsideScopeRollsBack(t *testing.T) {
	s, tag, _ := newAuditedTagStore(t)
	empty, err := s.NodeByPath(t.Context(), "/Empty")
	require.NoError(t, err)

	changed, err := s.AssignTag(t.Context(), tag.ID, empty.ID, empty.Revision)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	assert.Zero(t, changed)
	unchanged, err := s.NodeByID(t.Context(), empty.ID)
	require.NoError(t, err)
	assert.Equal(t, empty.Revision, unchanged.Revision)
	currentTag, err := s.TagByID(t.Context(), tag.ID)
	require.NoError(t, err)
	assert.Equal(t, tag.Revision, currentTag.Revision)
	assert.Zero(t, currentTag.AssignmentCount)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestAuditedTagAssignmentRollsBackWithHistory(t *testing.T) {
	s, tag, report := newAuditedTagStore(t)
	_, err := s.db.Exec(`CREATE TRIGGER reject_audit_tag_scope_advance
		BEFORE UPDATE ON audit_scopes BEGIN
		SELECT RAISE(ABORT, 'forced audit tag failure'); END`)
	require.NoError(t, err)

	changed, err := s.AssignTag(t.Context(), tag.ID, report.ID, report.Revision)
	require.ErrorContains(t, err, "forced audit tag failure")
	assert.Zero(t, changed)
	unchanged, err := s.NodeByID(t.Context(), report.ID)
	require.NoError(t, err)
	assert.Equal(t, report.Revision, unchanged.Revision)
	currentTag, err := s.TagByID(t.Context(), tag.ID)
	require.NoError(t, err)
	assert.Equal(t, tag.Revision, currentTag.Revision)
	assert.Zero(t, currentTag.AssignmentCount)
	var sequence int64
	require.NoError(t, s.db.QueryRow(
		`SELECT operation_sequence_high_water FROM audit_authority`,
	).Scan(&sequence))
	assert.Equal(t, int64(1), sequence)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestAuditedTagAssignmentReplayRejectsRetargetedNode(t *testing.T) {
	s, tag, report := newAuditedTagStore(t)
	_, err := s.AssignTag(t.Context(), tag.ID, report.ID, report.Revision)
	require.NoError(t, err)
	authority, scope, err := loadInitialAuditProjection(t.Context(), s.db)
	require.NoError(t, err)
	records, err := loadInitialAuditRecords(t.Context(), s.db)
	require.NoError(t, err)
	initial, err := selectInitialAuditRecords(authority, scope, records)
	require.NoError(t, err)
	replay, err := newAuditedHistoryReplay(authority, scope, initial)
	require.NoError(t, err)
	mutations, err := auditRecordsBySequence(records["canonical_mutation"], authority.sequence)
	require.NoError(t, err)
	mutation := mutations[2]
	digest, err := auditDigestField(mutation.record, "attached_metadata_change_digest")
	require.NoError(t, err)
	deltas := auditRecordsByDigest(records["attached_metadata_delta"])
	delta := deltas[digest]
	empty, err := s.NodeByPath(t.Context(), "/Empty")
	require.NoError(t, err)
	retargeted, err := auditTagAssignmentRecord(tag.ID, empty.ID)
	require.NoError(t, err)
	change, err := makeAttachedMetadataPresenceChange(retargeted, true)
	require.NoError(t, err)
	delta.record = mustReplaceAuditRecordField(
		t, delta.record, "changes", audit.List(audit.Nested(change)),
	)
	deltas[digest] = delta

	_, err = replay.validateTagAssignmentDelta(
		mutation.record, mustAuditUUIDField(t, mutation.record, auditOperationIDField),
		deltas, map[string]bool{},
	)
	require.ErrorContains(t, err, "unaudited node")
}

func TestAuditedTagAssignmentImportRejectsMissingCurrentAssignment(t *testing.T) {
	s, tag, report := newAuditedTagStore(t)
	_, err := s.AssignTag(t.Context(), tag.ID, report.ID, report.Revision)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	malformed := omitMetadataTagAssignment(t, exported.Bytes(), tag.ID, report.ID)

	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	err = restored.ImportMetadata(t.Context(), bytes.NewReader(malformed))
	require.ErrorContains(t, err, "replayed audit attachments do not match current metadata")
	var assignmentRows, auditRows int64
	require.NoError(t, restored.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM node_tags),
		(SELECT COUNT(*) FROM audit_records)`).Scan(&assignmentRows, &auditRows))
	assert.Zero(t, assignmentRows)
	assert.Zero(t, auditRows)
}

func newAuditedTagStore(t *testing.T) (*Store, Tag, Node) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	tag, err := s.CreateTag(t.Context(), "reviewed")
	require.NoError(t, err)
	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	report, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, projects.ID)
	return s, tag, report
}

func auditEventKindForSequence(t *testing.T, s *Store, sequence int64) string {
	t.Helper()
	records, err := loadInitialAuditRecords(t.Context(), s.db)
	require.NoError(t, err)
	mutations, err := auditRecordsBySequence(records["canonical_mutation"], sequence)
	require.NoError(t, err)
	events, err := auditRecordListField(mutations[sequence].record, "events")
	require.NoError(t, err)
	require.Len(t, events, 1)
	kind, err := auditTextField(events[0], "event_kind")
	require.NoError(t, err)
	return kind
}

func omitMetadataTagAssignment(
	t *testing.T, input []byte, tagID string, nodeID int64,
) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	kept := lines[:0]
	found := false
	for _, line := range lines {
		var header struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &header))
		if header.Type == "node_tag" {
			var assignment metadataNodeTag
			require.NoError(t, json.Unmarshal(line, &assignment))
			if assignment.TagID == tagID && assignment.NodeID == nodeID {
				found = true
				continue
			}
		}
		kept = append(kept, line)
	}
	require.True(t, found)
	return append(bytes.Join(kept, []byte{'\n'}), '\n')
}
