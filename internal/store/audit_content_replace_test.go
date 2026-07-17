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

func TestAuditedContentReplacementChainsMultipleOperations(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	file, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)

	for index, replacement := range []struct {
		hash string
		size int64
		mime string
	}{
		{fakeHash("d4"), 14, "text/markdown"},
		{fakeHash("e5"), 23, "application/pdf"},
	} {
		file, _, err = s.ReplaceContent(
			t.Context(), file.ID, file.Revision,
			replacement.hash, replacement.size, replacement.mime,
		)
		require.NoError(t, err, index)
	}

	var sequence, records int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT operation_sequence_high_water FROM audit_authority),
		(SELECT COUNT(*) FROM audit_records)`).Scan(&sequence, &records))
	assert.Equal(t, int64(3), sequence)
	assert.Equal(t, int64(16), records)
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
