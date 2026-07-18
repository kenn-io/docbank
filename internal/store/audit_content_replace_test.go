package store

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditedContentReplacementAdvancesPortableAuthority(t *testing.T) {
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	scope, err := source.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, source, scope.ID)
	file, err := source.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)

	updated, version, err := source.ReplaceContent(
		t.Context(), file.ID, file.Revision, fakeHash("d4"), 14, "text/markdown",
	)
	require.NoError(t, err)
	assert.Equal(t, file.Revision+1, updated.Revision)
	assert.Equal(t, version.ID, updated.CurrentVersionID)
	assert.Equal(t, "content_replace", version.TransitionKind)

	var sequence, allocationCount, scopeCount, recordCount int64
	require.NoError(t, source.db.QueryRow(`SELECT
		(SELECT operation_sequence_high_water FROM audit_authority),
		(SELECT allocation_entry_count FROM audit_authority),
		(SELECT entry_count FROM audit_scopes),
		(SELECT COUNT(*) FROM audit_records)`).Scan(
		&sequence, &allocationCount, &scopeCount, &recordCount,
	))
	assert.Equal(t, int64(2), sequence)
	assert.Equal(t, int64(2), allocationCount)
	assert.Equal(t, int64(2), scopeCount)
	assert.Equal(t, int64(12), recordCount)

	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	var roundTrip bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &roundTrip))
	assert.Equal(t, exported.Bytes(), roundTrip.Bytes())

	restoredFile, err := restored.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	assert.Equal(t, updated.CurrentVersionID, restoredFile.CurrentVersionID)
	versions, total, err := restored.ContentVersions(t.Context(), file.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 3, total)
	require.Len(t, versions, 3)
	assert.Equal(t, version.ID, versions[0].ID)
}

func TestAuditedContentRevertAdvancesPortableAuthority(t *testing.T) {
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	scope, err := source.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, source, scope.ID)
	file, err := source.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)

	updated, version, revertedFrom, err := source.RevertContent(
		t.Context(), file.ID, file.Revision, metadataVersionOld,
	)
	require.NoError(t, err)
	assert.Equal(t, file.Revision+1, updated.Revision)
	assert.Equal(t, version.ID, updated.CurrentVersionID)
	assert.Equal(t, "content_revert", version.TransitionKind)
	require.NotNil(t, version.SourceVersionID)
	assert.Equal(t, metadataVersionOld, *version.SourceVersionID)
	assert.Equal(t, metadataVersionOld, revertedFrom.ID)
	assert.Equal(t, revertedFrom.BlobHash, version.BlobHash)
	assert.Equal(t, revertedFrom.Size, version.Size)
	assert.Equal(t, revertedFrom.MimeType, version.MimeType)

	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	var roundTrip bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &roundTrip))
	assert.Equal(t, exported.Bytes(), roundTrip.Bytes())

	authority, auditScope, err := loadInitialAuditProjection(t.Context(), source.db)
	require.NoError(t, err)
	records, err := loadInitialAuditRecords(t.Context(), source.db)
	require.NoError(t, err)
	initial, err := selectInitialAuditRecords(authority, auditScope, records)
	require.NoError(t, err)
	replay, err := newAuditedHistoryReplay(authority, auditScope, initial)
	require.NoError(t, err)
	mutations, err := auditRecordsBySequence(records["canonical_mutation"], authority.sequence)
	require.NoError(t, err)
	events, err := auditRecordListField(mutations[2].record, "events")
	require.NoError(t, err)
	require.Len(t, events, 1)
	post, err := auditNestedField(events[0], "post")
	require.NoError(t, err)
	nodeID, err := auditUnsignedField(events[0], metadataNodeIDField)
	require.NoError(t, err)
	priorRevision, err := auditUnsignedField(replay.states[nodeID], "node_revision")
	require.NoError(t, err)
	priorVersionID, err := auditUUIDField(replay.versions[metadataVersionCurrent], "version_id")
	require.NoError(t, err)
	require.NoError(t, replay.validateContentTransitionSource(
		events[0], post, "content_revert", nodeID, priorVersionID, priorRevision,
	))
	tamperedPost, err := replaceAuditRecordField(
		post, "blob_hash", mustAuditDigest(t, fakeHash("deadbeef")),
	)
	require.NoError(t, err)
	require.ErrorContains(t, replay.validateContentTransitionSource(
		events[0], tamperedPost, "content_revert", nodeID, priorVersionID, priorRevision,
	), "bytes do not match")
}

func TestAuditedVaultRejectsRevertOutsideScope(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	empty, err := s.NodeByPath(t.Context(), "/Empty")
	require.NoError(t, err)
	outside, err := s.CreateFile(
		t.Context(), empty.ID, "outside.txt", fakeHash("aa11"), 11, "text/plain",
	)
	require.NoError(t, err)
	priorVersionID := outside.CurrentVersionID
	outside, _, err = s.ReplaceContent(
		t.Context(), outside.ID, outside.Revision, fakeHash("bb22"), 12, "text/plain",
	)
	require.NoError(t, err)
	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, projects.ID)

	revertedNode, revertedVersion, revertedSource, err := s.RevertContent(
		t.Context(), outside.ID, outside.Revision, priorVersionID,
	)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	assert.Zero(t, revertedNode)
	assert.Zero(t, revertedVersion)
	assert.Zero(t, revertedSource)
	unchanged, err := s.NodeByID(t.Context(), outside.ID)
	require.NoError(t, err)
	assert.Equal(t, outside, unchanged)
}

func TestAuditedContentRevertRollsBackWholeOperation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	file, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	_, err = s.db.Exec(`CREATE TRIGGER reject_audit_revert_scope_advance
		BEFORE UPDATE ON audit_scopes BEGIN
		SELECT RAISE(ABORT, 'forced audit revert failure'); END`)
	require.NoError(t, err)

	revertedNode, revertedVersion, revertedSource, err := s.RevertContent(
		t.Context(), file.ID, file.Revision, metadataVersionOld,
	)
	require.ErrorContains(t, err, "forced audit revert failure")
	assert.Zero(t, revertedNode)
	assert.Zero(t, revertedVersion)
	assert.Zero(t, revertedSource)
	unchanged, err := s.NodeByID(t.Context(), file.ID)
	require.NoError(t, err)
	assert.Equal(t, file, unchanged)
	var versions, records int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM content_versions WHERE node_id=?),
		(SELECT COUNT(*) FROM audit_records)`, file.ID).Scan(&versions, &records))
	assert.Equal(t, int64(2), versions)
	assert.Equal(t, int64(8), records)
}

func TestAuditedContentTransitionsChainMultipleOperations(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	file, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)

	var firstReplacementID string
	for index, replacement := range []struct {
		hash string
		size int64
		mime string
	}{
		{fakeHash("d4"), 14, "text/markdown"},
		{fakeHash("e5"), 23, "application/pdf"},
	} {
		var version ContentVersion
		file, version, err = s.ReplaceContent(
			t.Context(), file.ID, file.Revision,
			replacement.hash, replacement.size, replacement.mime,
		)
		require.NoError(t, err, index)
		if index == 0 {
			firstReplacementID = version.ID
		}
	}
	file, reverted, source, err := s.RevertContent(
		t.Context(), file.ID, file.Revision, firstReplacementID,
	)
	require.NoError(t, err)
	assert.Equal(t, firstReplacementID, source.ID)
	require.NotNil(t, reverted.SourceVersionID)
	assert.Equal(t, firstReplacementID, *reverted.SourceVersionID)
	assert.Equal(t, reverted.ID, file.CurrentVersionID)

	var sequence, records int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT operation_sequence_high_water FROM audit_authority),
		(SELECT COUNT(*) FROM audit_records)`).Scan(&sequence, &records))
	assert.Equal(t, int64(4), sequence)
	assert.Equal(t, int64(20), records)
	require.NoError(t, s.ExportMetadata(t.Context(), &bytes.Buffer{}))
}

func TestAuditedVaultRejectsReplacementOutsideScope(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	empty, err := s.NodeByPath(t.Context(), "/Empty")
	require.NoError(t, err)
	outside, err := s.CreateFile(
		t.Context(), empty.ID, "outside.txt", fakeHash("d4"), 14, "text/plain",
	)
	require.NoError(t, err)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	require.ErrorIs(t,
		s.CheckContentReplacementTarget(t.Context(), outside.ID, outside.Revision),
		ErrAuditMutationUnsupported,
	)

	_, _, err = s.ReplaceContent(
		t.Context(), outside.ID, outside.Revision, fakeHash("e5"), 23, "text/markdown",
	)
	require.ErrorContains(t, err, "is not in an audit scope")
	unchanged, err := s.NodeByID(t.Context(), outside.ID)
	require.NoError(t, err)
	assert.Equal(t, outside, unchanged)
	var candidateBlobs int64
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM blobs WHERE hash=?`, fakeHash("e5"),
	).Scan(&candidateBlobs))
	assert.Zero(t, candidateBlobs)
}

func TestAuditedContentReplacementImportRejectsFinalStateDivergence(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	file, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	_, _, err = s.ReplaceContent(
		t.Context(), file.ID, file.Revision, fakeHash("d4"), 14, "text/markdown",
	)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))

	divergent := mutateMetadataNodeRecord(t, exported.Bytes(), file.ID, func(node *metadataNode) {
		node.ModifiedAt = "2026-01-09T00:00:00.000000000Z"
	})
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	err = restored.ImportMetadata(t.Context(), bytes.NewReader(divergent))
	require.ErrorContains(t, err, "replayed audit topology does not match")
	var auditRows int64
	require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM audit_records`).Scan(&auditRows))
	assert.Zero(t, auditRows)
}

func TestAuditedContentReplacementRollsBackWholeOperation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	file, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	_, err = s.db.Exec(`CREATE TRIGGER reject_audit_scope_advance
		BEFORE UPDATE ON audit_scopes BEGIN
		SELECT RAISE(ABORT, 'forced audit replacement failure'); END`)
	require.NoError(t, err)

	_, _, err = s.ReplaceContent(
		t.Context(), file.ID, file.Revision, fakeHash("d4"), 14, "text/markdown",
	)
	require.ErrorContains(t, err, "forced audit replacement failure")

	unchanged, err := s.NodeByID(t.Context(), file.ID)
	require.NoError(t, err)
	assert.Equal(t, file, unchanged)
	var versions, records, candidateBlobs int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM content_versions WHERE node_id=?),
		(SELECT COUNT(*) FROM audit_records),
		(SELECT COUNT(*) FROM blobs WHERE hash=?)`, file.ID, fakeHash("d4")).Scan(
		&versions, &records, &candidateBlobs,
	))
	assert.Equal(t, int64(2), versions)
	assert.Equal(t, int64(8), records)
	assert.Zero(t, candidateBlobs)
}

func mutateMetadataNodeRecord(
	t *testing.T, input []byte, nodeID int64, mutate func(*metadataNode),
) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	for index, line := range lines {
		var kind struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &kind))
		if kind.Type != "node" {
			continue
		}
		var node metadataNode
		require.NoError(t, json.Unmarshal(line, &node))
		if node.ID != nodeID {
			continue
		}
		mutate(&node)
		var err error
		lines[index], err = json.Marshal(node)
		require.NoError(t, err)
		return append(bytes.Join(lines, []byte{'\n'}), '\n')
	}
	require.FailNow(t, "metadata node not found", nodeID)
	return nil
}
