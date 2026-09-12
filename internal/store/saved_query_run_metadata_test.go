package store

import (
	"bytes"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSavedQueryRunBackupRestorePreservesReceiptWithoutEphemeralHandle(t *testing.T) {
	source := newTestStore(t)
	definition, err := source.CreateSavedQuery(t.Context(), "root", "", SavedQueryKindQuery, []byte(`{}`))
	require.NoError(t, err)
	service := newQuerySnapshotService(source, querySnapshotServiceOptions{HMACKey: bytes.Repeat([]byte{0x51}, 32)})
	first, _, err := service.RunSaved(t.Context(), "owner", definition.ID, definition.Revision, SnapshotRequest{})
	require.NoError(t, err)
	run, _, err := service.RunSaved(t.Context(), "owner", definition.ID, definition.Revision, SnapshotRequest{})
	require.NoError(t, err)
	require.Equal(t, first.RunID, run.PreviousRunID)
	require.NoError(t, service.Close())

	snapshot, err := source.BeginMetadataSnapshot(t.Context())
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, snapshot.ExportBackup(t.Context(), &exported))
	require.NoError(t, snapshot.Close())
	assert.Contains(t, exported.String(), `"type":"saved_query_run"`)

	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	runs, err := restored.ListSavedQueryRuns(t.Context(), definition.ID, 10)
	require.NoError(t, err)
	require.Len(t, runs, 2)
	restoredRun := runs[0]
	assert.Equal(t, run, restoredRun)
	assert.Equal(t, run.MemberHash, restoredRun.MemberHash)
	var roundTrip bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &roundTrip))
	assert.Equal(t, exported.Bytes(), roundTrip.Bytes())
	restoredService := NewQuerySnapshotService(restored)
	t.Cleanup(func() { require.NoError(t, restoredService.Close()) })
	_, restoredSnapshotErr := restoredService.Page(t.Context(), "owner", restoredRun.SnapshotID, "")
	require.ErrorIs(t, restoredSnapshotErr, ErrSnapshotGone)
}

func TestSavedQueryRunMetadataImportRejectsMalformedReceiptTransactionally(t *testing.T) {
	source := newTestStore(t)
	definition, err := source.CreateSavedQuery(t.Context(), "root", "", SavedQueryKindQuery, []byte(`{}`))
	require.NoError(t, err)
	service := newQuerySnapshotService(source, querySnapshotServiceOptions{HMACKey: bytes.Repeat([]byte{0x53}, 32)})
	_, _, err = service.RunSaved(t.Context(), "owner", definition.ID, definition.Revision, SnapshotRequest{})
	require.NoError(t, err)
	require.NoError(t, service.Close())
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))
	malformed := mutateSavedQueryRunMetadataRecord(t, exported.String(), func(record metadataSavedQueryRun) metadataSavedQueryRun {
		record.MemberHash = "not-a-hash"
		return record
	})

	target := newTestStore(t)
	err = target.ImportMetadata(t.Context(), strings.NewReader(malformed))
	require.ErrorContains(t, err, "saved query run")
	runs, listErr := target.ListSavedQueryRuns(t.Context(), definition.ID, 10)
	require.NoError(t, listErr)
	assert.Empty(t, runs)
	_, lookupErr := target.SavedQueryByID(t.Context(), definition.ID)
	require.ErrorIs(t, lookupErr, ErrNotFound, "failed import must roll back the definition too")
}

func TestSavedQueryRunMetadataImportRejectsInventedPreviousReceipt(t *testing.T) {
	source := newTestStore(t)
	definition, err := source.CreateSavedQuery(t.Context(), "root", "", SavedQueryKindQuery, []byte(`{}`))
	require.NoError(t, err)
	service := newQuerySnapshotService(source, querySnapshotServiceOptions{HMACKey: bytes.Repeat([]byte{0x54}, 32)})
	_, _, err = service.RunSaved(t.Context(), "owner", definition.ID, definition.Revision, SnapshotRequest{})
	require.NoError(t, err)
	require.NoError(t, service.Close())
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))
	inventedID := "99999999-9999-4999-8999-999999999999"
	invented := mutateSavedQueryRunMetadataRecord(t, exported.String(), func(record metadataSavedQueryRun) metadataSavedQueryRun {
		record.PreviousRunID = &inventedID
		record.PreviousMemberHash = new(record.MemberHash)
		record.PreviousTotal = new(record.Total)
		record.PreviousQueryFingerprint = new(record.QueryFingerprint)
		return record
	})

	target := newTestStore(t)
	err = target.ImportMetadata(t.Context(), strings.NewReader(invented))
	require.ErrorContains(t, err, "previous receipt")
}

func TestSavedQueryRunMetadataImportRejectsDisconnectedComparisonChain(t *testing.T) {
	source := newTestStore(t)
	definition, err := source.CreateSavedQuery(t.Context(), "root", "", SavedQueryKindQuery, []byte(`{}`))
	require.NoError(t, err)
	service := newQuerySnapshotService(source, querySnapshotServiceOptions{HMACKey: bytes.Repeat([]byte{0x57}, 32)})
	_, _, err = service.RunSaved(t.Context(), "owner", definition.ID, definition.Revision, SnapshotRequest{})
	require.NoError(t, err)
	second, _, err := service.RunSaved(t.Context(), "owner", definition.ID, definition.Revision, SnapshotRequest{})
	require.NoError(t, err)
	require.NoError(t, service.Close())
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))
	disconnected := mutateSavedQueryRunMetadataRecordByID(t, exported.String(), second.RunID,
		func(record metadataSavedQueryRun) metadataSavedQueryRun {
			record.PreviousRunID = nil
			record.PreviousMemberHash = nil
			record.PreviousTotal = nil
			record.PreviousQueryFingerprint = nil
			return record
		})

	target := newTestStore(t)
	err = target.ImportMetadata(t.Context(), strings.NewReader(disconnected))
	require.ErrorContains(t, err, "comparison chain")
}

func TestSavedQueryRunMetadataValidationRejectsCorruptDatabaseAuthority(t *testing.T) {
	s := newTestStore(t)
	definition, err := s.CreateSavedQuery(t.Context(), "root", "", SavedQueryKindQuery, []byte(`{}`))
	require.NoError(t, err)
	service := newQuerySnapshotService(s, querySnapshotServiceOptions{HMACKey: bytes.Repeat([]byte{0x52}, 32)})
	run, _, err := service.RunSaved(t.Context(), "owner", definition.ID, definition.Revision, SnapshotRequest{})
	require.NoError(t, err)
	require.NoError(t, service.Close())
	_, err = s.db.Exec(`UPDATE saved_query_runs SET member_hash='broken' WHERE run_id=?`, run.RunID)
	require.NoError(t, err, "schema must leave evolving receipt validation to Go")
	err = s.ValidateMetadata(t.Context())
	require.ErrorContains(t, err, "saved query run")
	assert.Error(t, s.ExportMetadata(t.Context(), &bytes.Buffer{}))
}

func TestSavedQueryRunMetadataValidationRejectsPartialPreviousAuthority(t *testing.T) {
	s := newTestStore(t)
	definition, err := s.CreateSavedQuery(t.Context(), "root", "", SavedQueryKindQuery, []byte(`{}`))
	require.NoError(t, err)
	service := newQuerySnapshotService(s, querySnapshotServiceOptions{HMACKey: bytes.Repeat([]byte{0x55}, 32)})
	run, _, err := service.RunSaved(t.Context(), "owner", definition.ID, definition.Revision, SnapshotRequest{})
	require.NoError(t, err)
	require.NoError(t, service.Close())
	_, err = s.db.Exec(`UPDATE saved_query_runs SET previous_member_hash=member_hash WHERE run_id=?`, run.RunID)
	require.NoError(t, err)
	err = s.ValidateMetadata(t.Context())
	require.ErrorContains(t, err, "previous")
}

func TestSavedQueryRunMetadataValidationRejectsNoncanonicalTimestamp(t *testing.T) {
	s := newTestStore(t)
	definition, err := s.CreateSavedQuery(t.Context(), "root", "", SavedQueryKindQuery, []byte(`{}`))
	require.NoError(t, err)
	service := newQuerySnapshotService(s, querySnapshotServiceOptions{HMACKey: bytes.Repeat([]byte{0x56}, 32)})
	run, _, err := service.RunSaved(t.Context(), "owner", definition.ID, definition.Revision, SnapshotRequest{})
	require.NoError(t, err)
	require.NoError(t, service.Close())
	_, err = s.db.Exec(`UPDATE saved_query_runs SET ran_at='2026-09-11T12:00:00Z' WHERE run_id=?`, run.RunID)
	require.NoError(t, err)
	err = s.ValidateMetadata(t.Context())
	require.ErrorContains(t, err, "ran_at")
}

func mutateSavedQueryRunMetadataRecord(
	t *testing.T, input string, mutate func(metadataSavedQueryRun) metadataSavedQueryRun,
) string {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace([]byte(input)), []byte{'\n'})
	for index, line := range lines {
		var identity struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &identity))
		if identity.Type != metadataSavedQueryRunType {
			continue
		}
		var record metadataSavedQueryRun
		require.NoError(t, json.Unmarshal(line, &record))
		encoded, err := json.Marshal(mutate(record))
		require.NoError(t, err)
		lines[index] = encoded
		return string(append(bytes.Join(lines, []byte{'\n'}), '\n'))
	}
	require.FailNow(t, "saved query run metadata record not found")
	return ""
}

func mutateSavedQueryRunMetadataRecordByID(
	t *testing.T, input, id string, mutate func(metadataSavedQueryRun) metadataSavedQueryRun,
) string {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace([]byte(input)), []byte{'\n'})
	for index, line := range lines {
		var record metadataSavedQueryRun
		if err := json.Unmarshal(line, &record); err != nil || record.Type != metadataSavedQueryRunType || record.RunID != id {
			continue
		}
		encoded, err := json.Marshal(mutate(record))
		require.NoError(t, err)
		lines[index] = encoded
		return string(append(bytes.Join(lines, []byte{'\n'}), '\n'))
	}
	require.FailNow(t, "saved query run metadata record not found", id)
	return ""
}
