package store

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/audit"
)

func TestProvenanceBindingsPinAnExactVersion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	run, err := s.BeginIngest(t.Context(), "filesystem", "synthetic")
	require.NoError(t, err)
	const originalMtime = "2026-09-11T10:20:30.123456789Z"
	node, _, err := s.IngestFile(
		t.Context(), run, s.RootID(), "a.txt", fakeHash("a1"), 1,
		"text/plain", "a.txt", originalMtime,
	)
	require.NoError(t, err)
	firstVersionID := node.CurrentVersionID

	bindings, err := s.ProvenanceVersionBindings(t.Context(), firstVersionID)
	require.NoError(t, err)
	require.Len(t, bindings, 1)
	require.Equal(t, []ProvenanceVersionBinding{{
		ProvenanceIdentity: bindings[0].ProvenanceIdentity,
		ContentVersionID:   firstVersionID,
		ObservedAt:         run.record.StartedAt,
		BasisRef:           "ingest:exact-version",
	}}, bindings)
	require.NotEqual(t, originalMtime, bindings[0].ObservedAt)

	replaced, _, err := s.ReplaceContent(
		t.Context(), node.ID, node.Revision, fakeHash("a2"), 2, "text/plain",
	)
	require.NoError(t, err)
	require.NotEqual(t, firstVersionID, replaced.CurrentVersionID)
	bindings, err = s.ProvenanceVersionBindings(t.Context(), replaced.CurrentVersionID)
	require.NoError(t, err)
	require.Empty(t, bindings)
	bindings, err = s.ProvenanceVersionBindings(t.Context(), firstVersionID)
	require.NoError(t, err)
	require.Len(t, bindings, 1)

	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DELETE FROM nodes WHERE id=?`, node.ID)
		return err
	}))
	bindings, err = s.ProvenanceVersionBindings(t.Context(), firstVersionID)
	require.NoError(t, err)
	require.Empty(t, bindings)
}

func TestProvenanceBindingExistsWithoutMtime(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	run, err := s.BeginIngest(t.Context(), "filesystem", "synthetic")
	require.NoError(t, err)
	node, _, err := s.IngestFile(
		t.Context(), run, s.RootID(), "a.txt", fakeHash("a1"), 1,
		"text/plain", "a.txt", "",
	)
	require.NoError(t, err)
	bindings, err := s.ProvenanceVersionBindings(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, bindings, 1)
	require.NotEmpty(t, bindings[0].ObservedAt)
	require.Equal(t, "ingest:exact-version", bindings[0].BasisRef)
}

func TestProvenanceBindingsFollowRevertAndPruneLifecycle(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	firstRun, err := s.BeginIngest(t.Context(), "filesystem", "Synthetic first import")
	require.NoError(t, err)
	created, _, err := s.IngestFile(
		t.Context(), firstRun, s.RootID(), "history.txt", fakeHash("a31"), 3,
		"text/plain", "/synthetic/history.txt", "",
	)
	require.NoError(t, err)
	originalVersionID := created.CurrentVersionID
	originalBinding := bindingForIngest(t, s, originalVersionID, firstRun.ID())

	_, replacement, err := s.ReplaceContent(
		t.Context(), created.ID, created.Revision, fakeHash("b31"), 4, "text/plain",
	)
	require.NoError(t, err)
	secondRun, err := s.BeginIngest(t.Context(), "filesystem", "Synthetic second import")
	require.NoError(t, err)
	observed, added, err := s.IngestFileWithMembership(
		t.Context(), secondRun, s.RootID(), "history.txt", replacement.BlobHash,
		replacement.Size, replacement.MimeType, "/synthetic/history.txt", "",
	)
	require.NoError(t, err)
	require.False(t, added)
	replacementBinding := bindingForIngest(t, s, replacement.ID, secondRun.ID())

	reverted, revertVersion, source, err := s.RevertContent(
		t.Context(), created.ID, observed.Revision, originalVersionID,
	)
	require.NoError(t, err)
	require.Equal(t, originalVersionID, source.ID)
	require.Equal(t, revertVersion.ID, reverted.CurrentVersionID)
	bindings, err := s.ProvenanceVersionBindings(t.Context(), revertVersion.ID)
	require.NoError(t, err)
	require.Empty(t, bindings, "a revert is a new version and does not inherit source bindings")
	require.Equal(t, originalBinding,
		bindingForIngest(t, s, originalVersionID, firstRun.ID()))
	require.Equal(t, replacementBinding,
		bindingForIngest(t, s, replacement.ID, secondRun.ID()))

	receipt, err := s.PruneContentVersions(
		t.Context(), created.ID, reverted.Revision,
		VersionPruneSelector{VersionIDs: []string{replacement.ID}}, true,
	)
	require.NoError(t, err)
	require.Equal(t, 1, receipt.DeletedVersions)
	bindings, err = s.ProvenanceVersionBindings(t.Context(), replacement.ID)
	require.NoError(t, err)
	require.Empty(t, bindings, "pruning a version cascades only its exact bindings")
	require.Equal(t, originalBinding,
		bindingForIngest(t, s, originalVersionID, firstRun.ID()))
	bindings, err = s.ProvenanceVersionBindings(t.Context(), revertVersion.ID)
	require.NoError(t, err)
	require.Empty(t, bindings)
}

func TestProvenanceBindingWriterIsImmutableAndIdempotent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	run, err := s.BeginIngest(t.Context(), "filesystem", "synthetic")
	require.NoError(t, err)
	node, _, err := s.IngestFile(
		t.Context(), run, s.RootID(), "a.txt", fakeHash("a1"), 1,
		"text/plain", "a.txt", "",
	)
	require.NoError(t, err)
	binding := bindingForIngest(t, s, node.CurrentVersionID, run.ID())
	var revision int64
	require.NoError(t, s.db.QueryRow(`SELECT revision FROM document_event_dirty
		WHERE content_version_id=?`, node.CurrentVersionID).Scan(&revision))

	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return bindProvenanceVersionTx(t.Context(), tx, binding)
	}))
	var replayRevision int64
	require.NoError(t, s.db.QueryRow(`SELECT revision FROM document_event_dirty
		WHERE content_version_id=?`, node.CurrentVersionID).Scan(&replayRevision))
	require.Equal(t, revision, replayRevision)

	conflict := binding
	conflict.ObservedAt = "2026-09-10T00:00:00.000000000Z"
	err = s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return bindProvenanceVersionTx(t.Context(), tx, conflict)
	})
	require.ErrorContains(t, err, "provenance binding conflict")
	require.Equal(t, binding, bindingForIngest(t, s, node.CurrentVersionID, run.ID()))

	invalid := binding
	invalid.ObservedAt = "not-a-time"
	err = s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return bindProvenanceVersionTx(t.Context(), tx, invalid)
	})
	require.ErrorContains(t, err, "observation time")
	invalid = binding
	invalid.BasisRef = "inferred"
	err = s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return bindProvenanceVersionTx(t.Context(), tx, invalid)
	})
	require.ErrorContains(t, err, "invalid provenance binding basis")

	other, err := s.CreateFile(
		t.Context(), s.RootID(), "other.txt", fakeHash("b2"), 2, "text/plain",
	)
	require.NoError(t, err)
	invalid = binding
	invalid.ContentVersionID = other.CurrentVersionID
	err = s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return bindProvenanceVersionTx(t.Context(), tx, invalid)
	})
	require.ErrorIs(t, err, ErrProvenanceMismatch)

	_, err = s.db.Exec(`UPDATE provenance_version_bindings SET basis_ref='changed'
		WHERE provenance_identity=? AND content_version_id=?`,
		binding.ProvenanceIdentity, binding.ContentVersionID)
	require.ErrorContains(t, err, "immutable")
}

func TestProvenanceBindingsInGenesisRespectEnrollmentScope(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	inScope, err := s.Mkdir(t.Context(), s.RootID(), "in-scope")
	require.NoError(t, err)
	outOfScope, err := s.Mkdir(t.Context(), s.RootID(), "out-of-scope")
	require.NoError(t, err)
	run, err := s.BeginIngest(t.Context(), "filesystem", "synthetic")
	require.NoError(t, err)
	inNode, _, err := s.IngestFile(
		t.Context(), run, inScope.ID, "in.txt", fakeHash("11"), 1,
		"text/plain", "/synthetic/in.txt", "",
	)
	require.NoError(t, err)
	outNode, _, err := s.IngestFile(
		t.Context(), run, outOfScope.ID, "out.txt", fakeHash("22"), 2,
		"text/plain", "/synthetic/out.txt", "",
	)
	require.NoError(t, err)
	firstInVersion := inNode.CurrentVersionID
	inNode, _, err = s.ReplaceContent(
		t.Context(), inNode.ID, inNode.Revision, fakeHash("33"), 3, "text/plain",
	)
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, inScope.ID)

	records, err := loadInitialAuditRecords(t.Context(), s.db)
	require.NoError(t, err)
	genesis, err := auditRecordListField(records["attached_metadata_genesis"][0].record, "records")
	require.NoError(t, err)
	baseline, err := auditRecordListField(records["enrollment_baseline"][0].record, "attachments")
	require.NoError(t, err)
	require.Equal(t, 2, auditRecordKindCount(genesis, metadataProvenanceVersionBindingType))
	require.Equal(t, 1, auditRecordKindCount(baseline, metadataProvenanceVersionBindingType))
	require.Equal(t, firstInVersion,
		auditBindingVersionForProvenance(t, baseline,
			bindingForIngest(t, s, firstInVersion, run.ID()).ProvenanceIdentity))
	require.Empty(t, auditBindingVersionForProvenance(t, baseline,
		bindingForIngest(t, s, outNode.CurrentVersionID, run.ID()).ProvenanceIdentity))
	require.NotEqual(t, firstInVersion, inNode.CurrentVersionID)

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	var roundTrip bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &roundTrip))
	require.Equal(t, exported.Bytes(), roundTrip.Bytes())
}

func TestMetadataImportRejectsProvenanceBindingTamperingAndRollsBack(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	run, err := s.BeginIngest(t.Context(), "filesystem", "synthetic")
	require.NoError(t, err)
	node, _, err := s.IngestFile(
		t.Context(), run, scope.ID, "bound.txt", fakeHash("44"), 4,
		"text/plain", "/synthetic/bound.txt", "",
	)
	require.NoError(t, err)
	boundVersion := node.CurrentVersionID
	node, _, err = s.ReplaceContent(
		t.Context(), node.ID, node.Revision, fakeHash("55"), 5, "text/plain",
	)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	binding := firstMetadataBinding(t, exported.Bytes(), boundVersion)

	tests := []struct {
		name  string
		input []byte
		want  string
	}{
		{name: "removed", input: omitMetadataBinding(t, exported.Bytes(), boundVersion),
			want: "replayed audit attachments do not match current metadata"},
		{name: "unaudited addition", input: appendMetadataRecords(t, exported.Bytes(),
			func() metadataProvenanceVersionBinding {
				added := binding
				added.ContentVersionID = node.CurrentVersionID
				return added
			}()), want: "replayed audit attachments do not match current metadata"},
		{name: "changed time", input: mutateMetadataBinding(t, exported.Bytes(), boundVersion,
			func(record *metadataProvenanceVersionBinding) {
				record.ObservedAt = "2026-09-10T00:00:00.000000000Z"
			}), want: "replayed audit attachments do not match current metadata"},
		{name: "changed basis", input: mutateMetadataBinding(t, exported.Bytes(), boundVersion,
			func(record *metadataProvenanceVersionBinding) { record.BasisRef = "inferred" }),
			want: "invalid provenance binding basis"},
		{name: "other retained version", input: mutateMetadataBinding(t, exported.Bytes(), boundVersion,
			func(record *metadataProvenanceVersionBinding) {
				record.ContentVersionID = node.CurrentVersionID
			}), want: "replayed audit attachments do not match current metadata"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			target := newTestStore(t)
			err := target.ImportMetadata(t.Context(), bytes.NewReader(testCase.input))
			require.ErrorContains(t, err, testCase.want)
			var bindings, audits int64
			require.NoError(t, target.db.QueryRow(`SELECT
				(SELECT COUNT(*) FROM provenance_version_bindings),
				(SELECT COUNT(*) FROM audit_records)`).Scan(&bindings, &audits))
			require.Zero(t, bindings)
			require.Zero(t, audits)
		})
	}
}

func TestCurrentMetadataActiveAuditRequiresProvenanceBindingTable(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedInitialAuditAuthority(t, s, s.RootID())

	_, err := s.db.Exec(`DROP TABLE provenance_version_bindings`)
	require.NoError(t, err)

	var exported bytes.Buffer
	err = exportMetadataSnapshotWithVaultIdentity(
		t.Context(), s.db, &exported, currentMetadataLayout(),
	)
	require.ErrorContains(t, err, "no such table")
}

func auditRecordKindCount(records []audit.Record, kind string) int {
	count := 0
	for _, record := range records {
		if record.Kind == kind {
			count++
		}
	}
	return count
}

func auditBindingVersionForProvenance(t *testing.T, records []audit.Record, identity string) string {
	t.Helper()
	for _, record := range records {
		if record.Kind != metadataProvenanceVersionBindingType {
			continue
		}
		candidate, err := auditDigestField(record, "provenance_identity")
		require.NoError(t, err)
		if candidate != identity {
			continue
		}
		version, err := auditUUIDField(record, "content_version_id")
		require.NoError(t, err)
		return version
	}
	return ""
}

func firstMetadataBinding(t *testing.T, input []byte, versionID string) metadataProvenanceVersionBinding {
	t.Helper()
	for line := range bytes.SplitSeq(bytes.TrimSpace(input), []byte{'\n'}) {
		var record metadataProvenanceVersionBinding
		if err := json.Unmarshal(line, &record); err == nil &&
			record.Type == metadataProvenanceVersionBindingType && record.ContentVersionID == versionID {
			return record
		}
	}
	require.FailNow(t, "metadata binding not found", versionID)
	return metadataProvenanceVersionBinding{}
}

func mutateMetadataBinding(
	t *testing.T, input []byte, versionID string,
	mutate func(*metadataProvenanceVersionBinding),
) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	found := false
	for index, line := range lines {
		var record metadataProvenanceVersionBinding
		if err := json.Unmarshal(line, &record); err != nil ||
			record.Type != metadataProvenanceVersionBindingType || record.ContentVersionID != versionID {
			continue
		}
		mutate(&record)
		encoded, err := json.Marshal(record)
		require.NoError(t, err)
		lines[index] = encoded
		found = true
		break
	}
	require.True(t, found)
	return append(bytes.Join(lines, []byte{'\n'}), '\n')
}

func omitMetadataBinding(t *testing.T, input []byte, versionID string) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	kept := lines[:0]
	found := false
	for _, line := range lines {
		var record metadataProvenanceVersionBinding
		if err := json.Unmarshal(line, &record); err == nil &&
			record.Type == metadataProvenanceVersionBindingType && record.ContentVersionID == versionID {
			found = true
			continue
		}
		kept = append(kept, line)
	}
	require.True(t, found)
	return append(bytes.Join(kept, []byte{'\n'}), '\n')
}
