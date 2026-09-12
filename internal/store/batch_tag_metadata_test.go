package store

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBatchTagReceiptV1WireFormat(t *testing.T) {
	// Persisted receipt bytes are immutable and must outlive API response changes.
	const stored = `{"version":1,"operation_id":"11111111-1111-4111-8111-111111111111","request_digest":"9d29429ce1a103db51351b4d136306a00fb8dc77ae392b506ba09e625073c725","tag_id":"22222222-2222-4222-8222-222222222222","assign":true,"tag_revision":3,"assignment_count":2,"completed_at":"2026-09-11T12:00:00.000000000Z","nodes":[{"node_id":7,"expected_revision":3,"revision":4,"changed":true},{"node_id":9,"expected_revision":4,"revision":5,"changed":true}]}`
	receipt, err := decodeBatchTagReceiptV1([]byte(stored))
	require.NoError(t, err)
	require.Equal(t, validBatchTagReceiptForTest(), receipt)
}

func TestDecodeBatchTagReceiptRejectsOversizedInput(t *testing.T) {
	_, err := decodeBatchTagReceiptV1(bytes.Repeat([]byte{' '}, maxBatchTagReceiptJSONBytes+1))
	require.ErrorIs(t, err, ErrInvalidBatchTag)
}

func TestBatchTagReceiptRejectsImpossibleFinalTagState(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*BatchTagReceiptV1)
	}{
		{name: "tag revision does not exceed change count", mutate: func(r *BatchTagReceiptV1) {
			r.TagRevision = 2
		}},
		{name: "assign count omits a target", mutate: func(r *BatchTagReceiptV1) {
			r.AssignmentCount = 1
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			receipt := validBatchTagReceiptForTest()
			test.mutate(&receipt)
			encoded, err := json.Marshal(receipt, json.Deterministic(true))
			require.NoError(t, err)
			_, err = decodeBatchTagReceiptV1(encoded)
			require.ErrorIs(t, err, ErrInvalidBatchTag)
		})
	}
}

func TestBatchTagReceiptMetadataRejectsTamperingTransactionally(t *testing.T) {
	source := newTestStore(t)
	node, err := source.Mkdir(t.Context(), source.RootID(), "portable")
	require.NoError(t, err)
	tag, err := source.CreateTag(t.Context(), "Portable")
	require.NoError(t, err)
	_, err = source.BatchTags(t.Context(), BatchTagRequest{
		OperationID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		TagID:       tag.ID, Assign: true,
		Nodes: []BatchTagTarget{{NodeID: node.ID, Revision: node.Revision}},
	})
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))
	record := batchTagMetadataRecordFromExport(t, exported.Bytes())

	duplicate := appendMetadataRecords(t, exported.Bytes(), record)
	missingField := mutateBatchTagMetadataLine(t, exported.Bytes(), func(fields map[string]jsontext.Value) {
		delete(fields, auditOperationIDField)
	})
	unknownField := mutateBatchTagMetadataLine(t, exported.Bytes(), func(fields map[string]jsontext.Value) {
		fields["unexpected"] = jsontext.Value(`true`)
	})
	mismatchedOperation := mutateBatchTagMetadata(t, exported.Bytes(), func(record *metadataBatchTagReceipt) {
		record.OperationID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	})
	mismatchedDigest := mutateBatchTagMetadata(t, exported.Bytes(), func(record *metadataBatchTagReceipt) {
		record.RequestDigest = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	})
	noncanonicalReceipt := mutateBatchTagMetadata(t, exported.Bytes(), func(record *metadataBatchTagReceipt) {
		record.ReceiptJSON = append(record.ReceiptJSON, '\n')
	})
	invalidTransition := mutateBatchTagMetadata(t, exported.Bytes(), func(record *metadataBatchTagReceipt) {
		var receipt BatchTagReceiptV1
		require.NoError(t, json.Unmarshal(record.ReceiptJSON, &receipt))
		receipt.Nodes[0].Revision++
		record.ReceiptJSON, err = json.Marshal(receipt, json.Deterministic(true))
		require.NoError(t, err)
	})

	for _, test := range []struct {
		name  string
		input []byte
	}{
		{name: "duplicate", input: duplicate},
		{name: "missing field", input: missingField},
		{name: "unknown field", input: unknownField},
		{name: "mismatched operation", input: mismatchedOperation},
		{name: "mismatched digest", input: mismatchedDigest},
		{name: "noncanonical receipt", input: noncanonicalReceipt},
		{name: "invalid revision transition", input: invalidTransition},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := newTestStore(t)
			targetVaultID := target.VaultID()
			err := target.ImportMetadata(t.Context(), bytes.NewReader(test.input))
			require.Error(t, err)
			assert.Equal(t, targetVaultID, target.VaultID())
			var nodes, receipts int
			require.NoError(t, target.db.QueryRow(`SELECT
				(SELECT COUNT(*) FROM nodes),
				(SELECT COUNT(*) FROM batch_tag_receipts)`).Scan(&nodes, &receipts))
			assert.Equal(t, 1, nodes)
			assert.Zero(t, receipts)
		})
	}
}

func TestBatchTagReceiptStateValidationRejectsCorruptStoredBytes(t *testing.T) {
	s := newTestStore(t)
	node, err := s.Mkdir(t.Context(), s.RootID(), "corrupt")
	require.NoError(t, err)
	tag, err := s.CreateTag(t.Context(), "Corrupt")
	require.NoError(t, err)
	receipt, err := s.BatchTags(t.Context(), BatchTagRequest{
		OperationID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
		TagID:       tag.ID, Assign: true,
		Nodes: []BatchTagTarget{{NodeID: node.ID, Revision: node.Revision}},
	})
	require.NoError(t, err)
	_, err = s.db.Exec(`DROP TRIGGER batch_tag_receipts_immutable_update`)
	require.NoError(t, err, "simulate corruption outside the supported Store boundary")
	_, err = s.db.Exec(`UPDATE batch_tag_receipts SET receipt_json=receipt_json||x'0a'
		WHERE operation_id=?`, receipt.OperationID)
	require.NoError(t, err)
	err = s.ValidateMetadata(t.Context())
	require.ErrorContains(t, err, "not canonical")
	_, err = s.BatchTags(t.Context(), BatchTagRequest{
		OperationID: receipt.OperationID, TagID: tag.ID, Assign: true,
		Nodes: []BatchTagTarget{{NodeID: node.ID, Revision: node.Revision}},
	})
	require.ErrorContains(t, err, "not canonical")
}

func TestBatchTagReceiptRowsAreImmutable(t *testing.T) {
	s := newTestStore(t)
	node, err := s.Mkdir(t.Context(), s.RootID(), "immutable")
	require.NoError(t, err)
	tag, err := s.CreateTag(t.Context(), "Immutable")
	require.NoError(t, err)
	receipt, err := s.BatchTags(t.Context(), BatchTagRequest{
		OperationID: "13131313-1313-4313-8313-131313131313",
		TagID:       tag.ID, Assign: true,
		Nodes: []BatchTagTarget{{NodeID: node.ID, Revision: node.Revision}},
	})
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE batch_tag_receipts SET request_digest=request_digest
		WHERE operation_id=?`, receipt.OperationID)
	require.ErrorContains(t, err, "immutable")
	_, err = s.db.Exec(`DELETE FROM batch_tag_receipts WHERE operation_id=?`, receipt.OperationID)
	require.ErrorContains(t, err, "immutable")
	var rows int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM batch_tag_receipts
		WHERE operation_id=?`, receipt.OperationID).Scan(&rows))
	assert.Equal(t, 1, rows)
}

func TestBatchTagReceiptMakesMetadataTargetNonPristine(t *testing.T) {
	source := newTestStore(t)
	var emptyExport bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &emptyExport))
	target := newTestStore(t)
	node, err := target.Mkdir(t.Context(), target.RootID(), "existing")
	require.NoError(t, err)
	tag, err := target.CreateTag(t.Context(), "Existing")
	require.NoError(t, err)
	_, err = target.BatchTags(t.Context(), BatchTagRequest{
		OperationID: "ffffffff-ffff-4fff-8fff-ffffffffffff",
		TagID:       tag.ID, Assign: true,
		Nodes: []BatchTagTarget{{NodeID: node.ID, Revision: node.Revision}},
	})
	require.NoError(t, err)
	err = target.ImportMetadata(t.Context(), bytes.NewReader(emptyExport.Bytes()))
	require.ErrorContains(t, err, "not pristine")
	var receipts int
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM batch_tag_receipts`).Scan(&receipts))
	assert.Equal(t, 1, receipts)
}

func TestAuditedBatchTagReceiptSurvivesBackupMetadataSnapshot(t *testing.T) {
	source, tag, report := newAuditedTagStore(t)
	receipt, err := source.BatchTags(t.Context(), BatchTagRequest{
		OperationID: "12121212-1212-4212-8212-121212121212",
		TagID:       tag.ID, Assign: true,
		Nodes: []BatchTagTarget{{NodeID: report.ID, Revision: report.Revision}},
	})
	require.NoError(t, err)
	snapshot, err := source.BeginMetadataSnapshot(t.Context())
	require.NoError(t, err)
	var backup bytes.Buffer
	require.NoError(t, snapshot.ExportBackup(t.Context(), &backup))
	require.NoError(t, snapshot.Close())

	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(backup.Bytes())))
	require.NoError(t, restored.ValidateMetadata(t.Context()))
	replayed, err := restored.BatchTags(t.Context(), BatchTagRequest{
		OperationID: receipt.OperationID, TagID: tag.ID, Assign: true,
		Nodes: []BatchTagTarget{{NodeID: report.ID, Revision: report.Revision}},
	})
	require.NoError(t, err)
	assert.Equal(t, receipt, replayed)
	var roundTrip bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &roundTrip))
	assert.Equal(t, backup.Bytes(), roundTrip.Bytes())
}

func validBatchTagReceiptForTest() BatchTagReceiptV1 {
	return BatchTagReceiptV1{
		Version: 1, OperationID: "11111111-1111-4111-8111-111111111111",
		RequestDigest: "9d29429ce1a103db51351b4d136306a00fb8dc77ae392b506ba09e625073c725",
		TagID:         "22222222-2222-4222-8222-222222222222", Assign: true,
		TagRevision: 3, AssignmentCount: 2,
		CompletedAt: "2026-09-11T12:00:00.000000000Z",
		Nodes: []BatchTagNodeResultV1{
			{NodeID: 7, ExpectedRevision: 3, Revision: 4, Changed: true},
			{NodeID: 9, ExpectedRevision: 4, Revision: 5, Changed: true},
		},
	}
}

func batchTagMetadataRecordFromExport(t *testing.T, input []byte) metadataBatchTagReceipt {
	t.Helper()
	for line := range bytes.SplitSeq(bytes.TrimSpace(input), []byte{'\n'}) {
		var kind struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &kind))
		if kind.Type != metadataBatchTagReceiptType {
			continue
		}
		var record metadataBatchTagReceipt
		require.NoError(t, json.Unmarshal(line, &record))
		return record
	}
	require.FailNow(t, "batch tag receipt metadata record not found")
	return metadataBatchTagReceipt{}
}

func mutateBatchTagMetadata(
	t *testing.T, input []byte, mutate func(*metadataBatchTagReceipt),
) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	found := false
	for i, line := range lines {
		var kind struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &kind))
		if kind.Type != metadataBatchTagReceiptType {
			continue
		}
		var record metadataBatchTagReceipt
		require.NoError(t, json.Unmarshal(line, &record))
		mutate(&record)
		var err error
		lines[i], err = json.Marshal(record, json.Deterministic(true))
		require.NoError(t, err)
		found = true
		break
	}
	require.True(t, found)
	return append(bytes.Join(lines, []byte{'\n'}), '\n')
}

func mutateBatchTagMetadataLine(
	t *testing.T, input []byte, mutate func(map[string]jsontext.Value),
) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	found := false
	for i, line := range lines {
		var kind struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &kind))
		if kind.Type != metadataBatchTagReceiptType {
			continue
		}
		var fields map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(line, &fields))
		mutate(fields)
		var err error
		lines[i], err = json.Marshal(fields, json.Deterministic(true))
		require.NoError(t, err)
		found = true
		break
	}
	require.True(t, found)
	return append(bytes.Join(lines, []byte{'\n'}), '\n')
}
