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

func TestAuditedTagRenameRoundTripsAssignedDefinition(t *testing.T) {
	s, tag, report := newAuditedTagStore(t)
	assigned, err := s.AssignTag(t.Context(), tag.ID, report.ID, report.Revision)
	require.NoError(t, err)

	renamed, err := s.RenameTag(t.Context(), tag.ID, assigned.Tag.Revision, "needs review")
	require.NoError(t, err)
	assert.Equal(t, "needs review", renamed.Name)
	assert.Equal(t, assigned.Tag.Revision+1, renamed.Revision)
	current, err := s.NodeByID(t.Context(), report.ID)
	require.NoError(t, err)
	assert.Equal(t, assigned.Node.Revision+1, current.Revision)
	assert.Equal(t, "tag_rename", auditEventKindForSequence(t, s, 3))
	require.NoError(t, s.ValidateMetadata(t.Context()))

	assertAuditMetadataRoundTrip(t, s)
}

func TestAuditedTagRenameWithoutAuditedAssignmentsAdvancesOnlyAllocation(t *testing.T) {
	s, tag, _ := newAuditedTagStore(t)
	renamed, err := s.RenameTag(t.Context(), tag.ID, tag.Revision, "renamed")
	require.NoError(t, err)
	assert.Equal(t, "renamed", renamed.Name)

	var sequence, allocations, scopeEntries, mutations, deltas int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT operation_sequence_high_water FROM audit_authority),
		(SELECT allocation_entry_count FROM audit_authority),
		(SELECT entry_count FROM audit_scopes),
		(SELECT COUNT(*) FROM audit_records WHERE kind='canonical_mutation'),
		(SELECT COUNT(*) FROM audit_records WHERE kind='attached_metadata_delta')`,
	).Scan(&sequence, &allocations, &scopeEntries, &mutations, &deltas))
	assert.Equal(t, int64(2), sequence)
	assert.Equal(t, int64(2), allocations)
	assert.Equal(t, int64(1), scopeEntries)
	assert.Equal(t, int64(1), mutations)
	assert.Equal(t, int64(1), deltas)
	require.NoError(t, s.ValidateMetadata(t.Context()))
	assertAuditMetadataRoundTrip(t, s)
}

func TestAuditedTagRenamePreservesUnscopedTopology(t *testing.T) {
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

	_, err = s.RenameTag(t.Context(), tag.ID, assigned.Tag.Revision, "renamed")
	require.NoError(t, err)
	current, err := s.NodeByID(t.Context(), empty.ID)
	require.NoError(t, err)
	assert.Equal(t, assigned.Node.Revision+1, current.Revision)
	assert.Equal(t, assigned.Node.ModifiedAt, current.ModifiedAt)
	require.NoError(t, s.ValidateMetadata(t.Context()))
	assertAuditMetadataRoundTrip(t, s)
}

func TestAuditedTagRenameNoOpDoesNotAdvanceHistory(t *testing.T) {
	s, tag, _ := newAuditedTagStore(t)
	unchanged, err := s.RenameTag(t.Context(), tag.ID, tag.Revision, tag.Name)
	require.NoError(t, err)
	assert.Equal(t, tag, unchanged)

	var sequence int64
	require.NoError(t, s.db.QueryRow(
		`SELECT operation_sequence_high_water FROM audit_authority`,
	).Scan(&sequence))
	assert.Equal(t, int64(1), sequence)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestAuditedTagDefinitionChangesAcrossScopesFailClosed(t *testing.T) {
	tests := map[string]func(*Store, Tag) error{
		"rename": func(s *Store, tag Tag) error {
			_, err := s.RenameTag(t.Context(), tag.ID, tag.Revision, "renamed")
			return err
		},
		"delete": func(s *Store, tag Tag) error {
			_, err := s.DeleteTag(t.Context(), tag.ID, tag.Revision)
			return err
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			s, tag, first, second := newMultiScopeAuditedTagStore(t)
			var sequence int64
			require.NoError(t, s.db.QueryRow(
				`SELECT operation_sequence_high_water FROM audit_authority`,
			).Scan(&sequence))

			err := change(s, tag)
			require.ErrorIs(t, err, ErrAuditMutationUnsupported)
			current, err := s.TagByID(t.Context(), tag.ID)
			require.NoError(t, err)
			assert.Equal(t, tag, current)
			for _, prior := range []Node{first, second} {
				node, err := s.NodeByID(t.Context(), prior.ID)
				require.NoError(t, err)
				assert.Equal(t, prior, node)
			}
			var after int64
			require.NoError(t, s.db.QueryRow(
				`SELECT operation_sequence_high_water FROM audit_authority`,
			).Scan(&after))
			assert.Equal(t, sequence, after)
			require.NoError(t, s.ValidateMetadata(t.Context()))
		})
	}
}

func newMultiScopeAuditedTagStore(t *testing.T) (*Store, Tag, Node, Node) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	tag, err := s.CreateTag(t.Context(), "shared")
	require.NoError(t, err)
	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	firstPlan, err := s.PreviewInitialAudit(t.Context(), projects.ID, "api", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(t.Context(), firstPlan)
	require.NoError(t, err)
	empty, err := s.NodeByPath(t.Context(), "/Empty")
	require.NoError(t, err)
	secondPlan, err := s.PreviewInitialAudit(t.Context(), empty.ID, "api", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(t.Context(), secondPlan)
	require.NoError(t, err)
	report, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	first, err := s.AssignTag(t.Context(), tag.ID, report.ID, report.Revision)
	require.NoError(t, err)
	second, err := s.AssignTag(t.Context(), tag.ID, empty.ID, empty.Revision)
	require.NoError(t, err)
	return s, second.Tag, first.Node, second.Node
}

func TestAuditedTagRenameRollsBackDefinitionNodesAndHistory(t *testing.T) {
	s, tag, report := newAuditedTagStore(t)
	assigned, err := s.AssignTag(t.Context(), tag.ID, report.ID, report.Revision)
	require.NoError(t, err)
	_, err = s.db.Exec(`CREATE TRIGGER reject_audit_tag_rename_scope_advance
		BEFORE UPDATE ON audit_scopes BEGIN
		SELECT RAISE(ABORT, 'forced audit tag rename failure'); END`)
	require.NoError(t, err)

	_, err = s.RenameTag(t.Context(), tag.ID, assigned.Tag.Revision, "rollback")
	require.ErrorContains(t, err, "forced audit tag rename failure")
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

func TestAuditedTagRenameImportRejectsMismatchedCurrentDefinition(t *testing.T) {
	s, tag, _ := newAuditedTagStore(t)
	_, err := s.RenameTag(t.Context(), tag.ID, tag.Revision, "renamed")
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	malformed := rewriteMetadataTagName(t, exported.Bytes(), tag.ID, tag.Name)

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

func TestAuditedTagRenameReplayRejectsOmittedMemberEffect(t *testing.T) {
	s, tag, report := newAuditedTagStore(t)
	assigned, err := s.AssignTag(t.Context(), tag.ID, report.ID, report.Revision)
	require.NoError(t, err)
	_, err = s.RenameTag(t.Context(), tag.ID, assigned.Tag.Revision, "renamed")
	require.NoError(t, err)

	authority, scope, err := loadInitialAuditProjection(t.Context(), s.db)
	require.NoError(t, err)
	records, err := loadInitialAuditRecords(t.Context(), s.db)
	require.NoError(t, err)
	initial, err := selectInitialAuditRecords(authority, scope, records)
	require.NoError(t, err)
	replay, err := newAuditedHistoryReplay(authority, scope, initial)
	require.NoError(t, err)
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
	malformed := mutations[3]
	malformed.record = mustReplaceAuditRecordField(
		t, malformed.record, "events", audit.List(),
	)

	err = replay.applyTagDefinitionRename(
		s.vaultID, malformed, allocations[3], entries[3],
		deltas, events, usedDeltas, usedEvents,
	)
	require.ErrorContains(t, err, "event set does not match assigned audited nodes")
}

func assertAuditMetadataRoundTrip(t *testing.T, s *Store) {
	t.Helper()
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

func rewriteMetadataTagName(t *testing.T, input []byte, tagID, name string) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	found := false
	for index, line := range lines {
		var header struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &header))
		if header.Type != "tag" {
			continue
		}
		var definition metadataTag
		require.NoError(t, json.Unmarshal(line, &definition))
		if definition.ID != tagID {
			continue
		}
		definition.Name = name
		encoded, err := json.Marshal(definition)
		require.NoError(t, err)
		lines[index] = encoded
		found = true
	}
	require.True(t, found)
	return append(bytes.Join(lines, []byte{'\n'}), '\n')
}
