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

func TestAuditedNodeCreationInheritsMembershipAndRoundTrips(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)

	directory, err := s.Mkdir(t.Context(), scope.ID, "2026")
	require.NoError(t, err)
	file, err := s.CreateFile(
		t.Context(), directory.ID, "return.txt", fakeHash("ac1"), 31, "text/plain",
	)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(t.Context()))

	var sequence, allocationCount, scopeCount, baselineCount int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT operation_sequence_high_water FROM audit_authority),
		(SELECT allocation_entry_count FROM audit_authority),
		(SELECT entry_count FROM audit_scopes),
		(SELECT COUNT(*) FROM audit_baselines)`).Scan(
		&sequence, &allocationCount, &scopeCount, &baselineCount,
	))
	assert.Equal(t, int64(3), sequence)
	assert.Equal(t, sequence, allocationCount)
	assert.Equal(t, sequence, scopeCount)
	assert.Equal(t, int64(3), baselineCount)

	rows, err := s.db.Query(`SELECT node_id FROM audit_memberships
		WHERE node_id IN (?,?) ORDER BY node_id`, directory.ID, file.ID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var inherited []int64
	for rows.Next() {
		var nodeID int64
		require.NoError(t, rows.Scan(&nodeID))
		inherited = append(inherited, nodeID)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []int64{directory.ID, file.ID}, inherited)

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	var roundTrip bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &roundTrip))
	assert.Equal(t, exported.Bytes(), roundTrip.Bytes())
	restoredFile, err := restored.NodeByPath(t.Context(), "/Projects/2026/return.txt")
	require.NoError(t, err)
	assert.Equal(t, file.ID, restoredFile.ID)
	assert.Equal(t, file.CurrentVersionID, restoredFile.CurrentVersionID)
}

func TestAuditedNodeCreationImportRejectsMembershipRetarget(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	created, err := s.Mkdir(t.Context(), scope.ID, "retargeted")
	require.NoError(t, err)
	var initialBaseline string
	require.NoError(t, s.db.QueryRow(
		`SELECT digest FROM audit_baselines WHERE operation_id=?`, testAuditOperationID,
	).Scan(&initialBaseline))
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	malformed := retargetAuditMembership(
		t, exported.Bytes(), uint64(created.ID), initialBaseline,
	)

	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	err = restored.ImportMetadata(t.Context(), bytes.NewReader(malformed))
	require.ErrorContains(t, err, "membership")
	var auditRows int64
	require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM audit_records`).Scan(&auditRows))
	assert.Zero(t, auditRows)
}

func TestAuditedNodeCreationImportRejectsCrossVaultBaseline(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	_, err = s.Mkdir(t.Context(), scope.ID, "cross-vault")
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	malformed := rewriteCreatedBaselineVault(
		t, exported.Bytes(), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
	)

	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	err = restored.ImportMetadata(t.Context(), bytes.NewReader(malformed))
	require.ErrorContains(t, err, "enrollment_baseline.vault_id")
	var auditRows int64
	require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM audit_records`).Scan(&auditRows))
	assert.Zero(t, auditRows)
}

func TestAuditedNodeCreationRollsBackWholeOperation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	_, err = s.db.Exec(`CREATE TRIGGER reject_creation_scope_advance
		BEFORE UPDATE ON audit_scopes BEGIN
		SELECT RAISE(ABORT, 'forced audited creation failure'); END`)
	require.NoError(t, err)

	_, err = s.CreateFile(
		t.Context(), scope.ID, "rollback.txt", fakeHash("ac2"), 19, "text/plain",
	)
	require.ErrorContains(t, err, "forced audited creation failure")
	_, err = s.NodeByPath(t.Context(), "/Projects/rollback.txt")
	require.ErrorIs(t, err, ErrNotFound)
	var blobRows, baselineRows, recordRows int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM blobs WHERE hash=?),
		(SELECT COUNT(*) FROM audit_baselines),
		(SELECT COUNT(*) FROM audit_records)`, fakeHash("ac2")).Scan(
		&blobRows, &baselineRows, &recordRows,
	))
	assert.Zero(t, blobRows)
	assert.Equal(t, int64(1), baselineRows)
	assert.Equal(t, int64(8), recordRows)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func retargetAuditMembership(
	t *testing.T, input []byte, nodeID uint64, baselineDigest string,
) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	for index, line := range lines {
		var membership metadataAuditMembership
		if err := json.Unmarshal(line, &membership); err != nil ||
			membership.Type != metadataAuditMembershipType || membership.NodeID != nodeID {
			continue
		}
		membership.BaselineDigest = baselineDigest
		var err error
		lines[index], err = json.Marshal(membership)
		require.NoError(t, err)
		return append(bytes.Join(lines, []byte{'\n'}), '\n')
	}
	require.FailNow(t, "audit membership not found", nodeID)
	return nil
}

type auditRecordRewriteFixture struct {
	wrapper metadataAuditRecord
	record  audit.Record
}

func rewriteCreatedBaselineVault(t *testing.T, input []byte, vaultID string) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	records := make(map[string]auditRecordRewriteFixture)
	for _, line := range lines {
		var header struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &header))
		if header.Type != metadataAuditRecordType {
			continue
		}
		var wrapper metadataAuditRecord
		require.NoError(t, json.Unmarshal(line, &wrapper))
		record, err := audit.UnmarshalJSONRecord(wrapper.Record)
		require.NoError(t, err)
		records[wrapper.Digest] = auditRecordRewriteFixture{wrapper: wrapper, record: record}
	}

	rewrites := make(map[string]metadataAuditRecord)
	rewrite := func(oldDigest string, record audit.Record) string {
		raw, err := audit.MarshalJSONRecord(record)
		require.NoError(t, err)
		wrapper := records[oldDigest].wrapper
		wrapper.Digest, wrapper.Record = mustAuditRecordDigest(t, record), raw
		rewrites[oldDigest] = wrapper
		return wrapper.Digest
	}

	oldBaseline, baseline := findAuditRecord(t, records, func(record audit.Record) bool {
		if record.Kind != "enrollment_baseline" {
			return false
		}
		cause, err := auditTextField(record, "cause")
		return err == nil && cause == "node_create"
	})
	baseline, err := replaceAuditRecordField(baseline, auditVaultIDField, mustAuditUUID(t, vaultID))
	require.NoError(t, err)
	newBaseline := rewrite(oldBaseline, baseline)

	oldEventWrapper, eventWrapper := findAuditRecord(t, records, func(record audit.Record) bool {
		if record.Kind != auditEventField {
			return false
		}
		event, err := auditNestedField(record, auditEventField)
		if err != nil {
			return false
		}
		kind, err := auditTextField(event, "event_kind")
		if err != nil || kind != "audit_inherit" {
			return false
		}
		digest, err := auditDigestField(event, "baseline_digest")
		return err == nil && digest == oldBaseline
	})
	inheritedEvent, err := auditNestedField(eventWrapper, auditEventField)
	require.NoError(t, err)
	inheritedEvent, err = replaceAuditRecordField(
		inheritedEvent, "baseline_digest", mustAuditDigest(t, newBaseline),
	)
	require.NoError(t, err)
	eventWrapper, err = replaceAuditRecordField(
		eventWrapper, auditEventField, audit.Nested(inheritedEvent),
	)
	require.NoError(t, err)
	rewrite(oldEventWrapper, eventWrapper)

	oldMutation, mutation := findAuditRecord(t, records, func(record audit.Record) bool {
		if record.Kind != "canonical_mutation" {
			return false
		}
		bindings, err := auditRecordListField(record, "baselines")
		if err != nil || len(bindings) != 1 {
			return false
		}
		digest, err := auditDigestField(bindings[0], "baseline_digest")
		return err == nil && digest == oldBaseline
	})
	bindings, err := auditRecordListField(mutation, "baselines")
	require.NoError(t, err)
	bindings[0], err = replaceAuditRecordField(
		bindings[0], "baseline_digest", mustAuditDigest(t, newBaseline),
	)
	require.NoError(t, err)
	mutation, err = replaceAuditRecordField(
		mutation, "baselines", audit.List(audit.Nested(bindings[0])),
	)
	require.NoError(t, err)
	events, err := auditRecordListField(mutation, "events")
	require.NoError(t, err)
	wantEventID, err := auditDigestField(inheritedEvent, "event_id")
	require.NoError(t, err)
	for index := range events {
		eventID, eventErr := auditDigestField(events[index], "event_id")
		if eventErr == nil && eventID == wantEventID {
			events[index] = inheritedEvent
		}
	}
	mutation, err = replaceAuditRecordField(
		mutation, "events", audit.List(auditNestedValues(events)...),
	)
	require.NoError(t, err)
	newMutation := rewrite(oldMutation, mutation)

	oldScopeEntry, scopeEntry := findAuditRecord(t, records, func(record audit.Record) bool {
		if record.Kind != "scope_chain_entry" {
			return false
		}
		digest, err := auditDigestField(record, "mutation_hash")
		return err == nil && digest == oldMutation
	})
	scopeEntry, err = replaceAuditRecordField(
		scopeEntry, "mutation_hash", mustAuditDigest(t, newMutation),
	)
	require.NoError(t, err)
	newScopeEntry := rewrite(oldScopeEntry, scopeEntry)

	oldAllocation, allocation := findAuditRecord(t, records, func(record audit.Record) bool {
		if record.Kind != "allocation_entry" {
			return false
		}
		digest, err := auditDigestField(record, "mutation_hash")
		return err == nil && digest == oldMutation
	})
	allocation, err = replaceAuditRecordField(
		allocation, "mutation_hash", mustAuditDigest(t, newMutation),
	)
	require.NoError(t, err)
	newAllocation := rewrite(oldAllocation, allocation)

	for index, line := range lines {
		var header struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &header))
		switch header.Type {
		case metadataAuditRecordType:
			var wrapper metadataAuditRecord
			require.NoError(t, json.Unmarshal(line, &wrapper))
			if replacement, ok := rewrites[wrapper.Digest]; ok {
				lines[index], err = json.Marshal(replacement)
				require.NoError(t, err)
			}
		case metadataAuditMembershipType:
			var membership metadataAuditMembership
			require.NoError(t, json.Unmarshal(line, &membership))
			if membership.BaselineDigest == oldBaseline {
				membership.BaselineDigest = newBaseline
				lines[index], err = json.Marshal(membership)
				require.NoError(t, err)
			}
		case metadataAuditScopeType:
			var scope metadataAuditScope
			require.NoError(t, json.Unmarshal(line, &scope))
			if scope.ChainHead == oldScopeEntry {
				scope.ChainHead = newScopeEntry
				lines[index], err = json.Marshal(scope)
				require.NoError(t, err)
			}
		case metadataAuditAuthorityType:
			var authority metadataAuditAuthority
			require.NoError(t, json.Unmarshal(line, &authority))
			if authority.AllocationHead == oldAllocation {
				authority.AllocationHead = newAllocation
				lines[index], err = json.Marshal(authority)
				require.NoError(t, err)
			}
		}
	}
	return append(bytes.Join(lines, []byte{'\n'}), '\n')
}

func findAuditRecord(
	t *testing.T, records map[string]auditRecordRewriteFixture,
	match func(audit.Record) bool,
) (string, audit.Record) {
	t.Helper()
	for digest, fixture := range records {
		if match(fixture.record) {
			return digest, fixture.record
		}
	}
	require.FailNow(t, "matching audit record not found")
	return "", audit.Record{}
}
