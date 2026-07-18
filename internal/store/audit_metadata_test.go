package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/audit"
)

const (
	testAuditScopeID     = "66666666-6666-4666-8666-666666666666"
	testAuditOperationID = "77777777-7777-4777-8777-777777777777"
	testAuditLineageID   = "88888888-8888-4888-8888-888888888888"
	testAuditTimestamp   = "2026-07-17T12:34:56.123456789Z"
)

func TestInitialAuditAuthorityMetadataRoundTrip(t *testing.T) {
	ctx := context.Background()
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	target, err := source.NodeByPath(ctx, "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, source, target.ID)

	var first, second bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &first))
	require.NoError(t, source.ExportMetadata(ctx, &second))
	assert.Equal(t, first.Bytes(), second.Bytes())
	assert.Contains(t, first.String(), `"type":"audit_authority"`)
	assert.Contains(t, first.String(), `"type":"audit_scope"`)
	assert.Contains(t, first.String(), `"kind":"allocation_entry"`)

	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(first.Bytes())))
	var roundTrip bytes.Buffer
	require.NoError(t, restored.ExportMetadata(ctx, &roundTrip))
	assert.Equal(t, first.Bytes(), roundTrip.Bytes())

	var scopes, memberships, records int64
	require.NoError(t, restored.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM audit_scopes),
		(SELECT COUNT(*) FROM audit_memberships),
		(SELECT COUNT(*) FROM audit_records)`).Scan(&scopes, &memberships, &records))
	assert.Equal(t, int64(1), scopes)
	assert.Equal(t, int64(3), memberships, "scope adopts live descendants and retained origin trash")
	assert.Equal(t, int64(8), records)
}

func TestInitialAuditGenesisRejectsLiveSiblingCollision(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	report, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	other, err := s.CreateFile(
		t.Context(), projects.ID, "other.txt", fakeHash("a724"), 10, "text/plain",
	)
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, projects.ID)

	authority, scope, err := loadInitialAuditProjection(t.Context(), s.db)
	require.NoError(t, err)
	records, err := loadInitialAuditRecords(t.Context(), s.db)
	require.NoError(t, err)
	initial, err := selectInitialAuditRecords(authority, scope, records)
	require.NoError(t, err)
	topology := initial["topology_genesis"][0]
	nodes, err := auditRecordListField(topology.record, "nodes")
	require.NoError(t, err)
	for index, node := range nodes {
		if mustAuditUnsignedField(t, node, metadataNodeIDField) != uint64(report.ID) {
			continue
		}
		nodes[index] = mustReplaceAuditRecordField(
			t, node, "name", audit.Bytes([]byte(other.Name)),
		)
	}
	topology.record = mustReplaceAuditRecordField(
		t, topology.record, "nodes", audit.List(auditNestedValues(nodes)...),
	)

	err = validateInitialGenesis(
		s.VaultID(), authority, topology,
		initial["attached_metadata_genesis"][0], initial["allocation_genesis"][0],
	)
	require.ErrorContains(t, err, "same parent and name")
}

func TestInitialAuditAuthorityImportRejectsCorruptionAndRollsBack(t *testing.T) {
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	target, err := source.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, source, target.ID)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))

	corrupt := mutateAuditMetadataRecord(t, exported.Bytes(), metadataAuditAuthorityType, func(raw []byte) []byte {
		var authority metadataAuditAuthority
		require.NoError(t, json.Unmarshal(raw, &authority))
		authority.AllocationHead = metadataHashTrashed
		result, marshalErr := json.Marshal(authority)
		require.NoError(t, marshalErr)
		return result
	})
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	originalVaultID := restored.VaultID()
	err = restored.ImportMetadata(t.Context(), bytes.NewReader(corrupt))
	require.ErrorContains(t, err, "allocation authority does not match")
	assert.Equal(t, originalVaultID, restored.VaultID())
	var nodes, auditRows int64
	require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes))
	require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM audit_records`).Scan(&auditRows))
	assert.Equal(t, int64(1), nodes)
	assert.Zero(t, auditRows)
}

func TestInitialAuditAuthorityImportRejectsIncompleteMembership(t *testing.T) {
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	target, err := source.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, source, target.ID)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))

	incomplete := removeFirstAuditMetadataRecord(t, exported.Bytes(), metadataAuditMembershipType)
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	err = restored.ImportMetadata(t.Context(), bytes.NewReader(incomplete))
	require.ErrorContains(t, err, "membership projection does not match")
	var records int64
	require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM audit_records`).Scan(&records))
	assert.Zero(t, records)
}

func TestInitialAuditAuthorityImportActivatesAfterWholeStream(t *testing.T) {
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	target, err := source.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, source, target.ID)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))

	reordered := moveFirstMetadataRecordAfterHeader(t, exported.Bytes(), metadataAuditAuthorityType)
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(reordered)))
	require.NoError(t, restored.ExportMetadata(t.Context(), &bytes.Buffer{}))
}

func TestInitialAuditRootEnrollmentAdoptsUnknownOriginTrash(t *testing.T) {
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	_, err = source.db.Exec(`UPDATE nodes SET trash_parent=NULL WHERE id=11`)
	require.NoError(t, err)
	seedInitialAuditAuthority(t, source, source.RootID())

	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	var memberships int64
	require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM audit_memberships`).Scan(&memberships))
	assert.Equal(t, int64(5), memberships, "root scope adopts live nodes and unresolved retained trash")
}

func TestImportMetadataRejectsOrphanAuditRecord(t *testing.T) {
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))
	record := audit.Record{Kind: "scope_chain_entry", Fields: []audit.Field{
		{Name: "vault_id", Value: mustAuditUUID(t, source.VaultID())},
		{Name: auditScopeIDField, Value: mustAuditUUID(t, testAuditScopeID)},
		{Name: "entry_count", Value: audit.Unsigned(1)},
		{Name: "previous_head", Value: audit.Absent()},
		{Name: "mutation_hash", Value: mustAuditDigest(t, metadataHashCurrent)},
	}}
	recordJSON, err := audit.MarshalJSONRecord(record)
	require.NoError(t, err)
	input := appendMetadataRecords(t, exported.Bytes(), metadataAuditRecord{
		Type: metadataAuditRecordType, Digest: mustAuditRecordDigest(t, record), Record: recordJSON,
	})
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	err = restored.ImportMetadata(t.Context(), bytes.NewReader(input))
	require.ErrorContains(t, err, "dormant audit authority")
	var records int64
	require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM audit_records`).Scan(&records))
	assert.Zero(t, records)
}

func TestImportMetadataRejectsAuditRecordDigestMismatch(t *testing.T) {
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	target, err := source.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, source, target.ID)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))
	corrupt := mutateAuditMetadataRecord(t, exported.Bytes(), metadataAuditRecordType, func(raw []byte) []byte {
		var wrapper metadataAuditRecord
		require.NoError(t, json.Unmarshal(raw, &wrapper))
		var record map[string]any
		require.NoError(t, json.Unmarshal(wrapper.Record, &record))
		fields, ok := record["fields"].(map[string]any)
		require.True(t, ok)
		fields["lineage_id"] = "99999999-9999-4999-8999-999999999999"
		wrapper.Record, err = json.Marshal(record)
		require.NoError(t, err)
		result, err := json.Marshal(wrapper)
		require.NoError(t, err)
		return result
	})
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	err = restored.ImportMetadata(t.Context(), bytes.NewReader(corrupt))
	require.ErrorContains(t, err, "digest does not match")
	var records int64
	require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM audit_records`).Scan(&records))
	assert.Zero(t, records)
}

func TestAuditImportAppliesExactMetadataV1RecordSchemas(t *testing.T) {
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	seedMetadataRoundTrip(t, source)
	target, err := source.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, source, target.ID)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))

	malformed := mutateAuditMetadataRecord(
		t, exported.Bytes(), metadataAuditRecordType, func(raw []byte) []byte {
			var wrapper metadataAuditRecord
			require.NoError(t, json.Unmarshal(raw, &wrapper))
			var record map[string]any
			require.NoError(t, json.Unmarshal(wrapper.Record, &record))
			fields, ok := record["fields"].(map[string]any)
			require.True(t, ok)
			fields["future"] = nil
			wrapper.Record, err = json.Marshal(record)
			require.NoError(t, err)
			result, err := json.Marshal(wrapper)
			require.NoError(t, err)
			return result
		},
	)
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	err = restored.ImportMetadata(t.Context(), bytes.NewReader(malformed))
	require.ErrorContains(t, err, `contains unknown field "future"`)
}

func TestAuditValidationRejectsDirectDatabaseDivergence(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	target, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, target.ID)

	var wrongHead string
	require.NoError(t, s.db.QueryRow(
		`SELECT digest FROM audit_records WHERE kind='allocation_entry'`,
	).Scan(&wrongHead))
	_, err = s.db.Exec(`UPDATE audit_scopes SET chain_head=?`, wrongHead)
	require.NoError(t, err)
	err = s.ValidateMetadata(t.Context())
	require.ErrorContains(t, err, "audit scope authority does not match replayed history")
	err = s.ExportMetadata(t.Context(), &bytes.Buffer{})
	require.ErrorContains(t, err, "audit scope authority does not match replayed history")
}

func TestInitialAuditAuthorityRejectsUnsupportedLogicalMutations(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	target, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, target.ID)

	_, err = s.Mkdir(t.Context(), s.RootID(), "blocked")
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.CreateTag(t.Context(), "blocked")
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	run, err := s.BeginIngest(t.Context(), "cli", "blocked")
	require.NoError(t, err)
	_, _, err = s.IngestFile(
		t.Context(), run, s.RootID(), "blocked.txt", fakeHash("blocked"), 1,
		"text/plain", "/blocked.txt", "",
	)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.TrashEmpty(t.Context(), 0, true)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
}

func TestInitialAuditAuthorityAllowsReadOnlyMaintenancePreviews(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	target, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, target.ID)
	file, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)

	trashPreview, err := s.TrashEmpty(t.Context(), 0, false)
	require.NoError(t, err)
	assert.False(t, trashPreview.Run)
	prunePreview, err := s.PruneContentVersions(
		t.Context(), file.ID, file.Revision, VersionPruneSelector{KeepNewest: 1}, false,
	)
	require.NoError(t, err)
	assert.False(t, prunePreview.Run)
	assert.NotEmpty(t, prunePreview.Candidates)

	_, err = s.TrashEmpty(t.Context(), 0, true)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.PruneContentVersions(
		t.Context(), file.ID, file.Revision, VersionPruneSelector{KeepNewest: 1}, true,
	)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
}

func TestInitialAuditAuthorityAllowsUnreferencedBlobGC(t *testing.T) {
	const orphanHash = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	_, err = s.db.Exec(`INSERT INTO blobs(hash,size,created_at)
		VALUES(?,4,'2026-07-17T00:00:00.000000000Z')`, orphanHash)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO extracted_text(
		blob_hash,extractor,extractor_version,status,attempts,extracted_at)
		VALUES(?,'synthetic',1,'ok',1,'2026-07-17T00:00:00.000000000Z')`, orphanHash)
	require.NoError(t, err)
	target, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, target.ID)

	unreachable, err := s.UnreachableBlobs(t.Context())
	require.NoError(t, err)
	require.Equal(t, []BlobInfo{{Hash: orphanHash, Size: 4}}, unreachable)
	require.NoError(t, s.DeleteBlobRows(t.Context(), []string{orphanHash}))

	var rows int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM blobs WHERE hash=?) +
		(SELECT COUNT(*) FROM extracted_text WHERE blob_hash=?)`, orphanHash, orphanHash).Scan(&rows))
	assert.Zero(t, rows)
	require.NoError(t, s.ExportMetadata(t.Context(), &bytes.Buffer{}))
}

func TestInitialAuditAuthorityReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.db")
	s, err := Open(path)
	require.NoError(t, err)
	seedMetadataRoundTrip(t, s)
	target, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, target.ID)
	require.NoError(t, s.Close())

	reopened, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	require.NoError(t, reopened.ExportMetadata(t.Context(), &bytes.Buffer{}))
}

func seedInitialAuditAuthority(t *testing.T, s *Store, targetNodeID int64) {
	t.Helper()
	result, err := s.initializeAuditAuthorityWithInput(context.Background(), initialAuditEnrollmentInput{
		targetNodeID: targetNodeID,
		scopeID:      testAuditScopeID,
		operationID:  testAuditOperationID,
		lineageID:    testAuditLineageID,
		recordedAt:   testAuditTimestamp,
		origin:       "cli",
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.baselineDigest)
}

func mustAuditRecordDigest(t *testing.T, record audit.Record) string {
	t.Helper()
	digest, err := audit.Hash(record)
	require.NoError(t, err)
	return hex.EncodeToString(digest[:])
}

func mustAuditUUID(t *testing.T, value string) audit.Value {
	t.Helper()
	result, err := audit.UUID(value)
	require.NoError(t, err)
	return result
}

func mustAuditDigest(t *testing.T, value string) audit.Value {
	t.Helper()
	result, err := audit.DigestHex(value)
	require.NoError(t, err)
	return result
}

func mutateAuditMetadataRecord(
	t *testing.T, input []byte, kind string, mutate func([]byte) []byte,
) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	for index, line := range lines {
		var header struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &header))
		if header.Type == kind {
			lines[index] = mutate(line)
			return append(bytes.Join(lines, []byte{'\n'}), '\n')
		}
	}
	require.FailNow(t, "metadata record not found", kind)
	return nil
}

func removeFirstAuditMetadataRecord(t *testing.T, input []byte, kind string) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	for index, line := range lines {
		var header struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &header))
		if header.Type == kind {
			lines = append(lines[:index], lines[index+1:]...)
			return append(bytes.Join(lines, []byte{'\n'}), '\n')
		}
	}
	require.FailNow(t, "metadata record not found", kind)
	return nil
}

func moveFirstMetadataRecordAfterHeader(t *testing.T, input []byte, kind string) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	for index, line := range lines[1:] {
		var header struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &header))
		if header.Type == kind {
			index++
			result := make([][]byte, 0, len(lines))
			result = append(result, lines[0], lines[index])
			result = append(result, lines[1:index]...)
			result = append(result, lines[index+1:]...)
			return append(bytes.Join(result, []byte{'\n'}), '\n')
		}
	}
	require.FailNow(t, "metadata record not found", kind)
	return nil
}
