package store

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/audit"
)

func TestAuditedOperationalObservationsReplayOneOrTwoAttachments(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	priorRun, err := s.BeginIngest(t.Context(), "filesystem", "First synthetic import")
	require.NoError(t, err)
	priorSecond, added, err := s.IngestFile(
		t.Context(), priorRun, scope.ID, "second.txt", fakeHash("c3"), 9,
		"text/plain", "/synthetic/second.txt", "",
	)
	require.NoError(t, err)
	require.True(t, added)
	seedInitialAuditAuthority(t, s, scope.ID)

	run, err := s.BeginIngest(t.Context(), "filesystem", "Second synthetic import")
	require.NoError(t, err)
	observedFirst, added, err := s.IngestFileWithMembership(
		t.Context(), run, scope.ID, "report.txt", metadataHashCurrent, 12,
		"text/plain", "/synthetic/report.txt", "",
	)
	require.NoError(t, err)
	assert.False(t, added)
	assert.Equal(t, "ingest_observe", auditEventKindForSequence(t, s, 2))
	firstBinding := bindingForIngest(t, s, observedFirst.CurrentVersionID, run.ID())
	assert.Equal(t, auditMutationRecordedAt(t, s, 2), firstBinding.ObservedAt)
	assert.Equal(t, provenanceVersionBindingBasis, firstBinding.BasisRef)

	second, added, err := s.IngestFileWithMembership(
		t.Context(), run, scope.ID, "second.txt", fakeHash("c3"), 9,
		"text/plain", "/synthetic/second.txt", "",
	)
	require.NoError(t, err)
	assert.False(t, added)
	assert.Equal(t, priorSecond.ID, second.ID)
	assert.Equal(t, priorSecond.CurrentVersionID, second.CurrentVersionID)
	assert.Equal(t, priorSecond.Revision+1, second.Revision)
	assert.Equal(t, "ingest_observe", auditEventKindForSequence(t, s, 3))
	secondBinding := bindingForIngest(t, s, second.CurrentVersionID, run.ID())
	assert.Equal(t, auditMutationRecordedAt(t, s, 3), secondBinding.ObservedAt)

	var dirtyRevision int64
	require.NoError(t, s.db.QueryRow(`SELECT revision FROM document_event_dirty
		WHERE content_version_id=?`, second.CurrentVersionID).Scan(&dirtyRevision))
	replayed, added, err := s.IngestFileWithMembership(
		t.Context(), run, scope.ID, "second.txt", fakeHash("c3"), 9,
		"text/plain", "/synthetic/second.txt", "",
	)
	require.NoError(t, err)
	assert.False(t, added)
	assert.Equal(t, second, replayed)
	assert.Equal(t, secondBinding, bindingForIngest(t, s, second.CurrentVersionID, run.ID()))
	var replayDirtyRevision int64
	require.NoError(t, s.db.QueryRow(`SELECT revision FROM document_event_dirty
		WHERE content_version_id=?`, second.CurrentVersionID).Scan(&replayDirtyRevision))
	assert.Equal(t, dirtyRevision, replayDirtyRevision)

	var sequence int64
	require.NoError(t, s.db.QueryRow(
		`SELECT operation_sequence_high_water FROM audit_authority`,
	).Scan(&sequence))
	assert.Equal(t, int64(3), sequence)
	assert.Equal(t, []uint64{3, 2}, auditAttachmentCountsAfterEnrollment(t, s, 3))
	require.NoError(t, s.ValidateMetadata(t.Context()))

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

func bindingForIngest(t *testing.T, s *Store, versionID, ingestID string) ProvenanceVersionBinding {
	t.Helper()
	var binding ProvenanceVersionBinding
	require.NoError(t, s.db.QueryRow(`SELECT b.provenance_identity,b.content_version_id,
		b.observed_at,b.basis_ref FROM provenance_version_bindings b
		JOIN provenance p ON p.identity=b.provenance_identity
		WHERE b.content_version_id=? AND p.ingest_id=?`, versionID, ingestID).Scan(
		&binding.ProvenanceIdentity, &binding.ContentVersionID,
		&binding.ObservedAt, &binding.BasisRef,
	))
	return binding
}

func auditMutationRecordedAt(t *testing.T, s *Store, sequence int64) string {
	t.Helper()
	var raw []byte
	require.NoError(t, s.db.QueryRow(`SELECT record_json FROM audit_records
		WHERE kind='canonical_mutation' AND operation_sequence=?`, sequence).Scan(&raw))
	record, err := audit.UnmarshalJSONRecord(raw)
	require.NoError(t, err)
	value, err := auditTimestampField(record, auditRecordedAtField)
	require.NoError(t, err)
	return value
}

func mustAuditTimestamp(t *testing.T, value string) audit.Value {
	t.Helper()
	result, err := audit.Timestamp(value)
	require.NoError(t, err)
	return result
}

func auditAttachmentCountsAfterEnrollment(t *testing.T, s *Store, sequence int64) []uint64 {
	t.Helper()
	records, err := loadInitialAuditRecords(t.Context(), s.db)
	require.NoError(t, err)
	mutations, err := auditRecordsByOptionalSequence(records["canonical_mutation"], sequence)
	require.NoError(t, err)
	result := make([]uint64, 0, sequence-1)
	for current := int64(2); current <= sequence; current++ {
		count, err := auditUnsignedField(
			mutations[current].record, auditAttachedMetadataChangeCountField,
		)
		require.NoError(t, err)
		result = append(result, count)
	}
	return result
}

func TestOperationalObservationRejectsCallerSuppliedSource(t *testing.T) {
	s := newTestStore(t)
	run, err := s.BeginCallerSuppliedIngest(t.Context(), "agent", "Synthetic assertion")
	require.NoError(t, err)
	_, _, err = s.IngestFileWithMembership(
		t.Context(), run, s.RootID(), "note.txt", fakeHash("a1"), 4,
		"text/plain", "opaque://note", "",
	)
	require.ErrorContains(t, err, "rejects source kind")
	var ingests int64
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM ingests WHERE id=?`, run.ID()).Scan(&ingests))
	assert.Zero(t, ingests)
}

func TestAuditedReplacementForIngestRecordsBothTransitions(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	file, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	run, err := s.BeginIngest(t.Context(), "filesystem", "Replacement import")
	require.NoError(t, err)

	receipt, changed, err := s.ReplaceContentForIngest(
		t.Context(), run, file.ID, file.Revision, fakeHash("b2"), 8,
		"text/markdown", "/synthetic/report.txt", "",
	)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, file.Revision+2, receipt.Node.Revision)
	assert.Equal(t, receipt.Version.ID, receipt.Node.CurrentVersionID)
	binding := bindingForIngest(t, s, receipt.Version.ID, run.ID())
	assert.Equal(t, receipt.Version.ID, binding.ContentVersionID)
	var dirtyRevision int64
	require.NoError(t, s.db.QueryRow(`SELECT revision FROM document_event_dirty
		WHERE content_version_id=?`, receipt.Version.ID).Scan(&dirtyRevision))
	assert.Equal(t, []string{"content_replace", "ingest_observe"},
		auditEventKindsAfterEnrollment(t, s, 3))

	replayed, changed, err := s.ReplaceContentForIngest(
		t.Context(), run, receipt.Node.ID, receipt.Node.Revision, fakeHash("b2"), 8,
		"text/markdown", "/synthetic/report.txt", "",
	)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, receipt, replayed)
	assert.Equal(t, binding, bindingForIngest(t, s, receipt.Version.ID, run.ID()))
	var replayDirtyRevision int64
	require.NoError(t, s.db.QueryRow(`SELECT revision FROM document_event_dirty
		WHERE content_version_id=?`, receipt.Version.ID).Scan(&replayDirtyRevision))
	assert.Equal(t, dirtyRevision, replayDirtyRevision)
	assert.Equal(t, []string{"content_replace", "ingest_observe"},
		auditEventKindsAfterEnrollment(t, s, 3))
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func auditEventKindsAfterEnrollment(t *testing.T, s *Store, sequence int64) []string {
	t.Helper()
	records, err := loadInitialAuditRecords(t.Context(), s.db)
	require.NoError(t, err)
	mutations, err := auditRecordsByOptionalSequence(records["canonical_mutation"], sequence)
	require.NoError(t, err)
	result := make([]string, 0, sequence-1)
	for current := int64(2); current <= sequence; current++ {
		events, err := auditRecordListField(mutations[current].record, "events")
		require.NoError(t, err)
		require.Len(t, events, 1)
		kind, err := auditTextField(events[0], "event_kind")
		require.NoError(t, err)
		result = append(result, kind)
	}
	return result
}

func TestAuditedObservationRejectsInitialLabelWithoutChangingAuthority(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	file, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	label := "Forbidden label"
	run, err := s.BeginIngestWithLabel(t.Context(), "filesystem", "Labeled import", &label)
	require.NoError(t, err)

	_, _, err = s.IngestFileWithMembership(
		t.Context(), run, scope.ID, "report.txt", metadataHashCurrent, 12,
		"text/plain", "/synthetic/report.txt", "",
	)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	unchanged, err := s.NodeByID(t.Context(), file.ID)
	require.NoError(t, err)
	assert.Equal(t, file, unchanged)
	var sequence, ingests int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT operation_sequence_high_water FROM audit_authority),
		(SELECT COUNT(*) FROM ingests WHERE id=?)`, run.ID()).Scan(&sequence, &ingests))
	assert.Equal(t, int64(1), sequence)
	assert.Zero(t, ingests)
}

func TestAuditedObservationRollsBackRunFactAndRevision(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	file, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	var dirtyRevisionBefore int64
	require.NoError(t, s.db.QueryRow(`SELECT COALESCE((SELECT revision
		FROM document_event_dirty WHERE content_version_id=?),0)`,
		file.CurrentVersionID).Scan(&dirtyRevisionBefore))
	run, err := s.BeginIngest(t.Context(), "filesystem", "Rollback import")
	require.NoError(t, err)
	require.NoError(t, createAuditScopeFailureTrigger(s))

	_, _, err = s.IngestFileWithMembership(
		t.Context(), run, scope.ID, "report.txt", metadataHashCurrent, 12,
		"text/plain", "/synthetic/report.txt", "",
	)
	require.ErrorContains(t, err, "forced audited provenance failure")
	unchanged, err := s.NodeByID(t.Context(), file.ID)
	require.NoError(t, err)
	assert.Equal(t, file, unchanged)
	var sequence, ingests, facts, bindings, dirtyRevision int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT operation_sequence_high_water FROM audit_authority),
		(SELECT COUNT(*) FROM ingests WHERE id=?),
		(SELECT COUNT(*) FROM provenance WHERE ingest_id=?),
		(SELECT COUNT(*) FROM provenance_version_bindings b JOIN provenance p
		 ON p.identity=b.provenance_identity WHERE p.ingest_id=?),
		COALESCE((SELECT revision FROM document_event_dirty WHERE content_version_id=?),0)`,
		run.ID(), run.ID(), run.ID(), file.CurrentVersionID).Scan(
		&sequence, &ingests, &facts, &bindings, &dirtyRevision,
	))
	assert.Equal(t, int64(1), sequence)
	assert.Zero(t, ingests)
	assert.Zero(t, facts)
	assert.Zero(t, bindings)
	assert.Equal(t, dirtyRevisionBefore, dirtyRevision,
		"a rejected observation must not advance the exact version's dirty revision")
}

func TestIngestObservationReplayRejectsTampering(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scopeNode, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	observedNode, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scopeNode.ID)
	run, err := s.BeginIngest(t.Context(), "filesystem", "Synthetic replay")
	require.NoError(t, err)
	_, _, err = s.IngestFileWithMembership(
		t.Context(), run, scopeNode.ID, "report.txt", metadataHashCurrent, 12,
		"text/plain", "/synthetic/report.txt", "",
	)
	require.NoError(t, err)

	authority, scope, err := loadInitialAuditProjection(t.Context(), s.db)
	require.NoError(t, err)
	records, err := loadInitialAuditRecords(t.Context(), s.db)
	require.NoError(t, err)
	initial, err := selectInitialAuditRecords(authority, scope, records)
	require.NoError(t, err)
	mutations, err := auditRecordsByOptionalSequence(records["canonical_mutation"], authority.sequence)
	require.NoError(t, err)
	mutation := mutations[2]
	operationID, err := auditUUIDField(mutation.record, auditOperationIDField)
	require.NoError(t, err)
	deltas := auditRecordsByDigest(records["attached_metadata_delta"])
	digest, err := auditDigestField(mutation.record, "attached_metadata_change_digest")
	require.NoError(t, err)
	delta := deltas[digest]
	changes, err := auditRecordListField(delta.record, "changes")
	require.NoError(t, err)

	t.Run("trashed target", func(t *testing.T) {
		allocations, err := auditRecordsBySequence(records["allocation_entry"], authority.sequence)
		require.NoError(t, err)
		entries, err := auditScopeRecordsByScope(
			records["scope_chain_entry"], []initialAuditScope{scope},
		)
		require.NoError(t, err)
		events := mustAuditEventRecordsByID(t, records[auditEventField])

		liveReplay, err := newAuditedHistoryReplay(authority, scope, initial)
		require.NoError(t, err)
		require.NoError(t, liveReplay.applyProvenanceMutation(
			s.VaultID(), mutation, allocations[2], entries[scope.scopeID][2],
			deltas, events, map[string]bool{}, map[string]bool{},
		))

		trashReplay, err := newAuditedHistoryReplay(authority, scope, initial)
		require.NoError(t, err)
		nodeID := uint64(observedNode.ID)
		index, ok := trashReplay.topologyIndex[nodeID]
		require.True(t, ok)
		trashState, err := audit.Text(auditNodeStateTrash)
		require.NoError(t, err)
		trashReplay.topology[index] = mustReplaceAuditRecordField(
			t, trashReplay.topology[index], auditStateField, trashState,
		)
		priorState := trashReplay.states[nodeID]
		priorTopology := trashReplay.topology[index]
		priorScopeHead := trashReplay.scopeHead
		priorAllocationHead := trashReplay.allocationHead
		priorScopeCount := trashReplay.scopeEntryCount
		priorAllocationCount := trashReplay.allocationCount
		priorAttachmentCount := len(trashReplay.attachments)
		usedDeltas, usedEvents := map[string]bool{}, map[string]bool{}

		err = trashReplay.applyProvenanceMutation(
			s.VaultID(), mutation, allocations[2], entries[scope.scopeID][2],
			deltas, events, usedDeltas, usedEvents,
		)
		require.ErrorContains(t, err, "targets non-live node")
		assert.Equal(t, priorState, trashReplay.states[nodeID])
		assert.Equal(t, priorTopology, trashReplay.topology[index])
		assert.Equal(t, priorScopeHead, trashReplay.scopeHead)
		assert.Equal(t, priorAllocationHead, trashReplay.allocationHead)
		assert.Equal(t, priorScopeCount, trashReplay.scopeEntryCount)
		assert.Equal(t, priorAllocationCount, trashReplay.allocationCount)
		assert.Len(t, trashReplay.attachments, priorAttachmentCount)
		assert.False(t, usedDeltas[digest])
		assert.Empty(t, usedEvents)
	})

	t.Run("source kind", func(t *testing.T) {
		tampered := append([]audit.Record(nil), changes...)
		for index, change := range tampered {
			kind, err := auditTextField(change, "record_kind")
			require.NoError(t, err)
			if kind != metadataIngestType {
				continue
			}
			post, err := auditNestedField(change, auditPostField)
			require.NoError(t, err)
			post = mustReplaceAuditRecordField(t, post, "source_kind", mustAuditText(t, "embedded:filesystem"))
			tampered[index] = mustReplaceAuditRecordField(t, change, auditPostField, audit.Nested(post))
		}
		tamperedDelta := delta
		tamperedDelta.record = mustReplaceAuditRecordField(
			t, delta.record, "changes", audit.List(auditNestedValues(tampered)...),
		)
		replay, err := newAuditedHistoryReplay(authority, scope, initial)
		require.NoError(t, err)
		_, err = replay.validateIngestObservationDelta(
			mutation.record, operationID,
			map[string]storedAuditRecord{digest: tamperedDelta}, map[string]bool{},
		)
		require.ErrorContains(t, err, "requires an operational source kind")
	})

	for _, testCase := range []struct {
		name  string
		field string
		value audit.Value
	}{
		{name: "node identity", field: metadataNodeIDField, value: audit.Unsigned(999)},
		{name: "supersedes", field: "supersedes", value: mustAuditDigest(t, fakeHash("abcd"))},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			tampered := append([]audit.Record(nil), changes...)
			for index, change := range tampered {
				kind, err := auditTextField(change, "record_kind")
				require.NoError(t, err)
				if kind != metadataProvenanceType {
					continue
				}
				post, err := auditNestedField(change, auditPostField)
				require.NoError(t, err)
				post = mustReplaceAuditRecordField(t, post, testCase.field, testCase.value)
				tampered[index] = mustReplaceAuditRecordField(t, change, auditPostField, audit.Nested(post))
			}
			tamperedDelta := delta
			tamperedDelta.record = mustReplaceAuditRecordField(
				t, delta.record, "changes", audit.List(auditNestedValues(tampered)...),
			)
			replay, err := newAuditedHistoryReplay(authority, scope, initial)
			require.NoError(t, err)
			_, err = replay.validateIngestObservationDelta(
				mutation.record, operationID,
				map[string]storedAuditRecord{digest: tamperedDelta}, map[string]bool{},
			)
			require.Error(t, err)
		})
	}

	for _, testCase := range []struct {
		name  string
		field string
		value audit.Value
		want  string
	}{
		{name: "binding provenance identity", field: "provenance_identity",
			value: mustAuditDigest(t, fakeHash("dead")), want: "does not identify its observed fact"},
		{name: "binding exact version", field: "content_version_id",
			value: mustAuditUUID(t, "99999999-9999-4999-8999-999999999999"), want: "exact observed version"},
		{name: "binding observation time", field: "observed_at",
			value: mustAuditTimestamp(t, "2026-07-17T12:34:56.123456789Z"), want: "does not match its operation"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			tampered := append([]audit.Record(nil), changes...)
			for index, change := range tampered {
				kind, err := auditTextField(change, "record_kind")
				require.NoError(t, err)
				if kind != metadataProvenanceVersionBindingType {
					continue
				}
				post, err := auditNestedField(change, auditPostField)
				require.NoError(t, err)
				post = mustReplaceAuditRecordField(t, post, testCase.field, testCase.value)
				tampered[index], err = makeAttachedMetadataAddition(post)
				require.NoError(t, err)
			}
			tamperedDelta := delta
			tamperedDelta.record = mustReplaceAuditRecordField(
				t, delta.record, "changes", audit.List(auditNestedValues(tampered)...),
			)
			replay, err := newAuditedHistoryReplay(authority, scope, initial)
			require.NoError(t, err)
			_, err = replay.validateIngestObservationDelta(
				mutation.record, operationID,
				map[string]storedAuditRecord{digest: tamperedDelta}, map[string]bool{},
			)
			require.ErrorContains(t, err, testCase.want)
		})
	}

	t.Run("revision and version transition", func(t *testing.T) {
		for _, testCase := range []struct {
			name  string
			field string
			value audit.Value
		}{
			{name: "revision", field: "resulting_node_revision", value: audit.Unsigned(99)},
			{name: "version", field: "resulting_current_version_id", value: mustAuditUUID(t, "99999999-9999-4999-8999-999999999999")},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				replay, err := newAuditedHistoryReplay(authority, scope, initial)
				require.NoError(t, err)
				transition, err := replay.validateIngestObservationDelta(
					mutation.record, operationID, deltas, map[string]bool{},
				)
				require.NoError(t, err)
				events, err := auditRecordListField(mutation.record, "events")
				require.NoError(t, err)
				tamperedEvent := mustReplaceAuditRecordField(t, events[0], testCase.field, testCase.value)
				tamperedMutation := mustReplaceAuditRecordField(
					t, mutation.record, "events", audit.List(audit.Nested(tamperedEvent)),
				)
				eventID, err := auditDigestField(tamperedEvent, "event_id")
				require.NoError(t, err)
				wrappers := map[string]storedAuditRecord{eventID: {record: audit.Record{
					Kind:   auditEventField,
					Fields: []audit.Field{{Name: auditEventField, Value: audit.Nested(tamperedEvent)}},
				}}}
				err = replay.validateProvenanceMutationEvent(
					operationID, tamperedMutation, transition, wrappers, map[string]bool{},
				)
				require.Error(t, err)
			})
		}
	})

	t.Run("attached record count", func(t *testing.T) {
		replay, err := newAuditedHistoryReplay(authority, scope, initial)
		require.NoError(t, err)
		allocations, err := auditRecordsBySequence(records["allocation_entry"], authority.sequence)
		require.NoError(t, err)
		entries, err := auditScopeRecordsByScope(records["scope_chain_entry"], []initialAuditScope{scope})
		require.NoError(t, err)
		tampered := mutation
		tampered.record = mustReplaceAuditRecordField(
			t, mutation.record, auditAttachedMetadataChangeCountField, audit.Unsigned(1),
		)
		err = replay.applyProvenanceMutation(
			s.VaultID(), tampered, allocations[2], entries[scope.scopeID][2],
			deltas, mustAuditEventRecordsByID(t, records[auditEventField]), map[string]bool{}, map[string]bool{},
		)
		require.Error(t, err)
	})
}

func mustAuditEventRecordsByID(t *testing.T, records []storedAuditRecord) map[string]storedAuditRecord {
	t.Helper()
	result, err := auditEventRecordsByID(records)
	require.NoError(t, err)
	return result
}

func mustAuditText(t *testing.T, value string) audit.Value {
	t.Helper()
	result, err := audit.Text(value)
	require.NoError(t, err)
	return result
}
