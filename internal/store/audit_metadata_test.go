package store

import (
	"bytes"
	"context"
	"database/sql"
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
	require.ErrorContains(t, err, "initial allocation authority does not match")
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
		{Name: "scope_id", Value: mustAuditUUID(t, testAuditScopeID)},
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

func TestInitialAuditAuthorityBlocksPartialAndMutableState(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	target, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, target.ID)

	_, err = s.db.Exec(`UPDATE audit_records SET record_json='{}'`)
	require.ErrorContains(t, err, "audit records are immutable")
	_, err = s.db.Exec(`DELETE FROM audit_memberships`)
	require.ErrorContains(t, err, "audit memberships are immutable")
	var wrongHead string
	require.NoError(t, s.db.QueryRow(
		`SELECT digest FROM audit_records WHERE kind='allocation_entry'`).Scan(&wrongHead))
	_, err = s.db.Exec(`UPDATE audit_scopes SET chain_head=?`, wrongHead)
	require.ErrorContains(t, err, "audited metadata is read-only")
	require.NoError(t, s.ExportMetadata(t.Context(), &bytes.Buffer{}))
}

func TestInitialAuditAuthorityFreezesLogicalMetadata(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	target, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, target.ID)

	statements := []string{
		`UPDATE nodes SET name=name WHERE id=1`,
		`UPDATE content_versions SET mime_type=mime_type WHERE node_id=10`,
		`INSERT INTO blobs(hash,size,created_at) VALUES('dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd',1,'2026-07-17T00:00:00.000000000Z')`,
		`INSERT INTO ingests(id,started_at,source_kind,source_desc) VALUES('99999999-9999-4999-8999-999999999999','2026-07-17T00:00:00.000000000Z','cli','blocked')`,
		`INSERT INTO provenance(identity,node_id,ingest_id,original_path) VALUES('dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd',10,'44444444-4444-4444-8444-444444444444','/blocked')`,
		`INSERT INTO tags(id,name) VALUES('99999999-9999-4999-8999-999999999999','blocked')`,
		`INSERT INTO node_tags(node_id,tag_id) VALUES(7,'55555555-5555-4555-8555-555555555555')`,
		`UPDATE node_tags SET node_id=7 WHERE node_id=10`,
	}
	for _, statement := range statements {
		_, err := s.db.Exec(statement)
		require.ErrorContains(t, err, "audited metadata is read-only", statement)
	}

	_, err = s.Mkdir(t.Context(), s.RootID(), "blocked")
	require.ErrorContains(t, err, "audited metadata is read-only")
	_, err = s.CreateTag(t.Context(), "blocked")
	require.ErrorContains(t, err, "audited metadata is read-only")
	_, err = s.BeginIngest(t.Context(), "cli", "blocked")
	require.ErrorContains(t, err, "audited metadata is read-only")
	_, err = s.TrashEmpty(t.Context(), 0, true)
	require.ErrorContains(t, err, "audited metadata is read-only")
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
	ctx := context.Background()
	topology, err := currentAuditTopology(ctx, s.db)
	require.NoError(t, err)
	attachments, err := currentAuditAttachments(ctx, s.db)
	require.NoError(t, err)
	var nodeSequence int64
	require.NoError(t, s.db.QueryRow(
		`SELECT seq FROM sqlite_sequence WHERE name='nodes'`).Scan(&nodeSequence))

	topologyGenesis := audit.Record{Kind: "topology_genesis", Fields: []audit.Field{
		{Name: "vault_id", Value: mustAuditUUID(t, s.VaultID())},
		{Name: "lineage_id", Value: mustAuditUUID(t, testAuditLineageID)},
		{Name: "nodes", Value: audit.List(auditNestedValues(topology)...)},
	}}
	attachmentGenesis := audit.Record{Kind: "attached_metadata_genesis", Fields: []audit.Field{
		{Name: "vault_id", Value: mustAuditUUID(t, s.VaultID())},
		{Name: "lineage_id", Value: mustAuditUUID(t, testAuditLineageID)},
		{Name: "records", Value: audit.List(auditNestedValues(attachments)...)},
	}}
	topologyDigest := mustAuditRecordDigest(t, topologyGenesis)
	attachmentDigest := mustAuditRecordDigest(t, attachmentGenesis)
	allocationGenesis := audit.Record{Kind: "allocation_genesis", Fields: []audit.Field{
		{Name: "vault_id", Value: mustAuditUUID(t, s.VaultID())},
		{Name: "lineage_id", Value: mustAuditUUID(t, testAuditLineageID)},
		{Name: "previous_head", Value: audit.Absent()},
		{Name: "node_id_high_water", Value: audit.Unsigned(uint64(nodeSequence))},
		{Name: "operation_sequence_high_water", Value: audit.Unsigned(0)},
		{Name: "topology_count", Value: audit.Unsigned(uint64(len(topology)))},
		{Name: "topology_digest", Value: mustAuditDigest(t, topologyDigest)},
		{Name: "attached_metadata_count", Value: audit.Unsigned(uint64(len(attachments)))},
		{Name: "attached_metadata_digest", Value: mustAuditDigest(t, attachmentDigest)},
	}}
	allocationGenesisDigest := mustAuditRecordDigest(t, allocationGenesis)

	members, err := deriveInitialAuditMembers(ctx, s.db, uint64(targetNodeID))
	require.NoError(t, err)
	states, err := currentAuditMemberStates(ctx, s.db, members)
	require.NoError(t, err)
	versions, err := currentAuditVersions(ctx, s.db, members)
	require.NoError(t, err)
	baselineAttachments, err := auditRecordsForNodes(attachments, auditMemberSet(members))
	require.NoError(t, err)
	baselineNodes, witnesses, err := initialBaselineTopology(topology, members, testAuditOperationID)
	require.NoError(t, err)
	baseline := makeAuditEnrollmentBaseline(t, s.VaultID(), targetNodeID, members,
		states, versions, baselineAttachments, baselineNodes, witnesses)
	baselineDigest := mustAuditRecordDigest(t, baseline)
	event := makeInitialAuditEvent(t, targetNodeID, baselineDigest, states)
	eventWrapper := audit.Record{Kind: "event", Fields: []audit.Field{
		{Name: "event", Value: audit.Nested(event)},
	}}
	mutation := makeInitialAuditMutation(t, s.VaultID(), targetNodeID, baselineDigest, event)
	mutationDigest := mustAuditRecordDigest(t, mutation)
	scopeEntry := audit.Record{Kind: "scope_chain_entry", Fields: []audit.Field{
		{Name: "vault_id", Value: mustAuditUUID(t, s.VaultID())},
		{Name: "scope_id", Value: mustAuditUUID(t, testAuditScopeID)},
		{Name: "entry_count", Value: audit.Unsigned(1)},
		{Name: "previous_head", Value: audit.Absent()},
		{Name: "mutation_hash", Value: mustAuditDigest(t, mutationDigest)},
	}}
	allocationEntry := makeInitialAllocationEntry(t, s.VaultID(), nodeSequence,
		allocationGenesisDigest, mutationDigest)

	records := []audit.Record{topologyGenesis, attachmentGenesis, allocationGenesis,
		baseline, eventWrapper, mutation, scopeEntry, allocationEntry}
	require.NoError(t, s.withTx(ctx, func(tx *sql.Tx) error {
		for _, record := range records {
			if err := insertAuditRecordFixture(tx, record); err != nil {
				return err
			}
		}
		scopeHead := mustAuditRecordDigest(t, scopeEntry)
		allocationHead := mustAuditRecordDigest(t, allocationEntry)
		if _, err := tx.Exec(`INSERT INTO audit_authority VALUES(1,?,?,?,1,?)`,
			testAuditLineageID, 1, allocationGenesisDigest, allocationHead); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO audit_scopes VALUES(?,?,?,1,?)`,
			testAuditScopeID, targetNodeID, testAuditOperationID, scopeHead); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO audit_baselines VALUES(?,?,?,?)`,
			baselineDigest, testAuditScopeID, targetNodeID, testAuditOperationID); err != nil {
			return err
		}
		for _, member := range members {
			if _, err := tx.Exec(`INSERT INTO audit_memberships VALUES(?,?,?)`,
				testAuditScopeID, member, baselineDigest); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`INSERT INTO audit_write_guard VALUES(1)`)
		return err
	}))
}

func makeAuditEnrollmentBaseline(
	t *testing.T, vaultID string, targetNodeID int64, members []uint64,
	states, versions, attachments, nodes, witnesses []audit.Record,
) audit.Record {
	t.Helper()
	memberValues := make([]audit.Value, len(members))
	for index, member := range members {
		memberValues[index] = audit.Unsigned(member)
	}
	return audit.Record{Kind: "enrollment_baseline", Fields: []audit.Field{
		{Name: "vault_id", Value: mustAuditUUID(t, vaultID)},
		{Name: "scope_id", Value: mustAuditUUID(t, testAuditScopeID)},
		{Name: "target_node_id", Value: audit.Unsigned(uint64(targetNodeID))},
		{Name: "operation_id", Value: mustAuditUUID(t, testAuditOperationID)},
		{Name: "cause", Value: mustAuditText(t, "explicit")},
		{Name: "members", Value: audit.List(memberValues...)},
		{Name: "member_states", Value: audit.List(auditNestedValues(states)...)},
		{Name: "nodes", Value: audit.List(auditNestedValues(nodes)...)},
		{Name: "versions", Value: audit.List(auditNestedValues(versions)...)},
		{Name: "attachments", Value: audit.List(auditNestedValues(attachments)...)},
		{Name: "witnesses", Value: audit.List(auditNestedValues(witnesses)...)},
	}}
}

func makeInitialAuditEvent(
	t *testing.T, targetNodeID int64, baselineDigest string, states []audit.Record,
) audit.Record {
	t.Helper()
	operation := mustAuditUUID(t, testAuditOperationID)
	eventIdentity := audit.Record{Kind: "event_identity", Fields: []audit.Field{
		{Name: "operation_id", Value: operation},
		{Name: "event_ordinal", Value: audit.Unsigned(0)},
	}}
	identityDigest := mustAuditRecordDigest(t, eventIdentity)
	var targetState audit.Record
	for _, state := range states {
		id, err := auditUnsignedField(state, metadataNodeIDField)
		require.NoError(t, err)
		if id == uint64(targetNodeID) {
			targetState = state
		}
	}
	require.NotEmpty(t, targetState.Kind)
	revision, err := auditUnsignedField(targetState, "node_revision")
	require.NoError(t, err)
	current, err := auditField(targetState, "current_version_id")
	require.NoError(t, err)
	return audit.Record{Kind: "audit_event", Fields: []audit.Field{
		{Name: "event_id", Value: mustAuditDigest(t, identityDigest)},
		{Name: "operation_id", Value: operation},
		{Name: metadataNodeIDField, Value: audit.Unsigned(uint64(targetNodeID))},
		{Name: "event_kind", Value: mustAuditText(t, "audit_enroll")},
		{Name: "scope_id", Value: mustAuditUUID(t, testAuditScopeID)},
		{Name: "target_node_id", Value: audit.Unsigned(uint64(targetNodeID))},
		{Name: "attachment_kind", Value: audit.Absent()},
		{Name: "attachment_identity", Value: audit.Absent()},
		{Name: "source_version_id", Value: audit.Absent()},
		{Name: "event_ordinal", Value: audit.Unsigned(0)},
		{Name: "recorded_at", Value: mustAuditTimestamp(t, testAuditTimestamp)},
		{Name: "prior_node_revision", Value: audit.Unsigned(revision)},
		{Name: "resulting_node_revision", Value: audit.Unsigned(revision)},
		{Name: "prior_current_version_id", Value: current},
		{Name: "resulting_current_version_id", Value: current},
		{Name: "origin", Value: mustAuditText(t, "cli")},
		{Name: "agent_label", Value: audit.Absent()},
		{Name: "pre", Value: audit.Absent()},
		{Name: "post", Value: audit.Absent()},
		{Name: "topology_delta", Value: audit.Absent()},
		{Name: "baseline_digest", Value: mustAuditDigest(t, baselineDigest)},
	}}
}

func makeInitialAuditMutation(
	t *testing.T, vaultID string, targetNodeID int64, baselineDigest string,
	event audit.Record,
) audit.Record {
	t.Helper()
	binding := audit.Record{Kind: "baseline_binding", Fields: []audit.Field{
		{Name: "scope_id", Value: mustAuditUUID(t, testAuditScopeID)},
		{Name: "target_node_id", Value: audit.Unsigned(uint64(targetNodeID))},
		{Name: "baseline_digest", Value: mustAuditDigest(t, baselineDigest)},
	}}
	return audit.Record{Kind: "canonical_mutation", Fields: []audit.Field{
		{Name: "vault_id", Value: mustAuditUUID(t, vaultID)},
		{Name: "operation_sequence", Value: audit.Unsigned(1)},
		{Name: "operation_id", Value: mustAuditUUID(t, testAuditOperationID)},
		{Name: "grouping_id", Value: audit.Absent()},
		{Name: "recorded_at", Value: mustAuditTimestamp(t, testAuditTimestamp)},
		{Name: "origin", Value: mustAuditText(t, "cli")},
		{Name: "agent_label", Value: audit.Absent()},
		{Name: "events", Value: audit.List(audit.Nested(event))},
		{Name: "member_state_changes", Value: audit.List()},
		{Name: "baselines", Value: audit.List(audit.Nested(binding))},
		{Name: "topology_delta", Value: audit.Absent()},
		{Name: "path_effect_count", Value: audit.Unsigned(0)},
		{Name: "path_effect_digest", Value: audit.Absent()},
		{Name: "witness_change_count", Value: audit.Unsigned(0)},
		{Name: "witness_change_digest", Value: audit.Absent()},
		{Name: "attached_metadata_change_count", Value: audit.Unsigned(0)},
		{Name: "attached_metadata_change_digest", Value: audit.Absent()},
	}}
}

func makeInitialAllocationEntry(
	t *testing.T, vaultID string, nodeSequence int64, genesisDigest, mutationDigest string,
) audit.Record {
	t.Helper()
	return audit.Record{Kind: "allocation_entry", Fields: []audit.Field{
		{Name: "vault_id", Value: mustAuditUUID(t, vaultID)},
		{Name: "lineage_id", Value: mustAuditUUID(t, testAuditLineageID)},
		{Name: "previous_head", Value: mustAuditDigest(t, genesisDigest)},
		{Name: "operation_sequence", Value: audit.Unsigned(1)},
		{Name: "operation_id", Value: mustAuditUUID(t, testAuditOperationID)},
		{Name: "allocated_node_ids", Value: audit.List()},
		{Name: "node_id_high_water", Value: audit.Unsigned(uint64(nodeSequence))},
		{Name: "operation_sequence_high_water", Value: audit.Unsigned(1)},
		{Name: "has_audited_mutation", Value: audit.Bool(true)},
		{Name: "mutation_hash", Value: mustAuditDigest(t, mutationDigest)},
		{Name: "has_topology_change", Value: audit.Bool(false)},
		{Name: "topology_delta", Value: audit.Absent()},
		{Name: "has_witness_change", Value: audit.Bool(false)},
		{Name: "witness_change_count", Value: audit.Unsigned(0)},
		{Name: "witness_change_digest", Value: audit.Absent()},
		{Name: "has_attached_metadata_change", Value: audit.Bool(false)},
		{Name: "attached_metadata_change_count", Value: audit.Unsigned(0)},
		{Name: "attached_metadata_change_digest", Value: audit.Absent()},
	}}
}

func insertAuditRecordFixture(tx *sql.Tx, record audit.Record) error {
	digestValue, err := audit.Hash(record)
	if err != nil {
		return err
	}
	digest := hex.EncodeToString(digestValue[:])
	recordJSON, err := audit.MarshalJSONRecord(record)
	if err != nil {
		return err
	}
	index, err := indexAuditRecord(record)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO audit_records(digest,kind,operation_id,operation_sequence,
		scope_id,entry_count,event_id,event_ordinal,record_json) VALUES(?,?,?,?,?,?,?,?,?)`,
		digest, record.Kind, index.operationID, index.operationSequence, index.scopeID,
		index.entryCount, index.eventID, index.eventOrdinal, string(recordJSON))
	return err
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

func mustAuditText(t *testing.T, value string) audit.Value {
	t.Helper()
	result, err := audit.Text(value)
	require.NoError(t, err)
	return result
}

func mustAuditTimestamp(t *testing.T, value string) audit.Value {
	t.Helper()
	result, err := audit.Timestamp(value)
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
