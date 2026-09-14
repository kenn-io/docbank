package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestDocumentEventCoverageReportUsesOnlyFreshCurrentFileHeads(t *testing.T) {
	s := newTestStore(t)

	empty, err := s.DocumentEventCoverageReport(t.Context())
	require.NoError(t, err)
	assert.Zero(t, empty.Selected)
	assert.Zero(t, documentEventStateCount(t, s), "an empty report must remain read-only")

	node, err := s.CreateFile(t.Context(), s.RootID(), "current.txt", fakeHash("a11"), 1, "text/plain")
	require.NoError(t, err)
	oldVersionID := node.CurrentVersionID
	oldTarget := requireDocumentEventTarget(t, s, oldVersionID)
	oldRecord := coverageDocumentEventRecord(t, s.VaultID(), oldVersionID, "a12", "created", false)
	_, err = s.PublishDocumentEvents(t.Context(), oldTarget, DocumentEventsDeriverFingerprint,
		requireDocumentEventInputsSHA256(t, s, oldTarget), mustMarshalDocumentEvents(t, oldRecord))
	require.NoError(t, err)

	node, current, err := s.ReplaceContent(t.Context(), node.ID, node.Revision,
		fakeHash("b21"), 1, "text/plain")
	require.NoError(t, err)
	require.Equal(t, current.ID, node.CurrentVersionID)

	coverage, err := s.DocumentEventCoverageReport(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), coverage.Selected)
	assert.Zero(t, coverage.Indexed, "an indexed retained version must not cover its replacement")
	assert.Equal(t, int64(1), coverage.Pending)
	assert.Equal(t, int64(1), coverage.MissingMetadata)
	assert.Equal(t, int64(1), coverage.UnboundProvenance)

	currentTarget := requireDocumentEventTarget(t, s, current.ID)
	currentRecord := coverageDocumentEventRecord(t, s.VaultID(), current.ID,
		"b22", "vault_recorded", true)
	currentDigest := requireDocumentEventInputsSHA256(t, s, currentTarget)
	_, err = s.PublishDocumentEvents(t.Context(), currentTarget, DocumentEventsDeriverFingerprint,
		currentDigest, mustMarshalDocumentEvents(t, currentRecord))
	require.NoError(t, err)

	coverage, err = s.DocumentEventCoverageReport(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), coverage.Indexed)
	assert.Zero(t, coverage.Pending)
	assert.Equal(t, int64(1), coverage.InvalidDates)
	assert.Equal(t, int64(1), coverage.OperationalFallbacks)

	_, err = s.db.Exec(`INSERT INTO document_event_dirty(content_version_id,revision,reason)
		VALUES(?,1,'test')`, current.ID)
	require.NoError(t, err)
	coverage, err = s.DocumentEventCoverageReport(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), coverage.Pending, "a mismatched dirty revision is pending")
	_, err = s.db.Exec(`DELETE FROM document_event_dirty WHERE content_version_id=?`, current.ID)
	require.NoError(t, err)

	_, err = s.db.Exec(`UPDATE document_event_attempts SET inputs_sha256=?
		WHERE content_version_id=?`, fakeHash("b23"), current.ID)
	require.NoError(t, err)
	coverage, err = s.DocumentEventCoverageReport(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), coverage.Pending, "a head and attempt digest mismatch is pending")
	_, err = s.db.Exec(`UPDATE document_event_attempts SET inputs_sha256=?
		WHERE content_version_id=?`, currentDigest, current.ID)
	require.NoError(t, err)

	_, err = s.db.Exec(`DELETE FROM document_event_heads WHERE content_version_id=?`, current.ID)
	require.NoError(t, err)
	coverage, err = s.DocumentEventCoverageReport(t.Context())
	require.NoError(t, err)
	assert.Zero(t, coverage.Indexed, "an attempt without its matching head is pending")
	assert.Equal(t, int64(1), coverage.Pending)
	assert.Zero(t, coverage.InvalidDates, "only fresh attempt diagnostics are counted")
	assert.Zero(t, coverage.OperationalFallbacks, "only a fresh selected primary counts")
}

func TestDocumentEventCoverageReportsFreshTerminalStates(t *testing.T) {
	s := newTestStore(t)
	failed, err := s.CreateFile(t.Context(), s.RootID(), "failed.txt", fakeHash("e51"), 1, "text/plain")
	require.NoError(t, err)
	unavailable, err := s.CreateFile(t.Context(), s.RootID(), "unavailable.txt", fakeHash("e52"), 1, "text/plain")
	require.NoError(t, err)
	_, err = s.StartDocumentEventRebuild(t.Context(),
		"90000000-0000-4000-8000-000000000009", fakeHash("e53"))
	require.NoError(t, err)
	failedTarget := requireDocumentEventTarget(t, s, failed.CurrentVersionID)
	unavailableTarget := requireDocumentEventTarget(t, s, unavailable.CurrentVersionID)
	require.NoError(t, s.RecordDocumentEventAttempt(t.Context(), failedTarget,
		requireDocumentEventInputsSHA256(t, s, failedTarget), "failed", []byte(`[]`)))
	require.NoError(t, s.RecordDocumentEventAttempt(t.Context(), unavailableTarget,
		requireDocumentEventInputsSHA256(t, s, unavailableTarget), "unavailable", []byte(`[]`)))

	coverage, err := s.DocumentEventCoverageReport(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(2), coverage.Selected)
	assert.Equal(t, int64(1), coverage.Failed)
	assert.Equal(t, int64(1), coverage.Unavailable)
	assert.Zero(t, coverage.Pending)

	_, err = s.BumpDocumentEventInputEpoch(t.Context(), DocumentEventsDeriverFingerprint)
	require.NoError(t, err)
	coverage, err = s.DocumentEventCoverageReport(t.Context())
	require.NoError(t, err)
	assert.Zero(t, coverage.Failed)
	assert.Zero(t, coverage.Unavailable)
	assert.Equal(t, int64(2), coverage.Pending, "terminal attempts from an older epoch are pending")
}

func TestOlderCompleteReceiptCanCoexistWithNewerPendingCurrentCoverage(t *testing.T) {
	s := newTestStore(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "epoch-relative.txt", fakeHash("c31"), 1, "text/plain")
	require.NoError(t, err)
	older, err := s.StartDocumentEventRebuild(t.Context(),
		"60000000-0000-4000-8000-000000000006", fakeHash("c32"))
	require.NoError(t, err)
	require.Equal(t, int64(2), older.TargetEpoch)
	target := requireDocumentEventTarget(t, s, node.CurrentVersionID)
	record := coverageDocumentEventRecord(t, s.VaultID(), node.CurrentVersionID,
		"c33", "created", false)
	_, err = s.PublishDocumentEvents(t.Context(), target, DocumentEventsDeriverFingerprint,
		requireDocumentEventInputsSHA256(t, s, target), mustMarshalDocumentEvents(t, record))
	require.NoError(t, err)

	newer, err := s.StartDocumentEventRebuild(t.Context(),
		"70000000-0000-4000-8000-000000000007", fakeHash("c34"))
	require.NoError(t, err)
	require.Equal(t, int64(3), newer.TargetEpoch)
	require.NoError(t, s.RefreshDocumentEventBuilds(t.Context()))

	older, err = s.DocumentEventBuild(t.Context(), older.OperationID)
	require.NoError(t, err)
	assert.Equal(t, "completed", older.State,
		"a matching attempt at the receipt target epoch completes that receipt")
	newer, err = s.DocumentEventBuild(t.Context(), newer.OperationID)
	require.NoError(t, err)
	assert.Equal(t, "running", newer.State)
	coverage, err := s.DocumentEventCoverageReport(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(3), coverage.InputEpoch)
	assert.Equal(t, int64(1), coverage.Pending,
		"current coverage remains pending until the newest epoch is published")
	assert.Zero(t, coverage.Indexed)
}

func TestDocumentEventCoverageRequiresF10MetadataContract(t *testing.T) {
	s := newTestStore(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "metadata-contract.txt", fakeHash("d41"), 1, "text/plain")
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO source_metadata_generations(
		generation_id,source_sha256,contract_version,extractor_fingerprint,
		canonical_json,checksum,created_at) VALUES(?,?,?,?,?,?,?)`,
		"80000000-0000-4000-8000-000000000008", node.BlobHash, "not-f10",
		fakeHash("d42"), []byte(`{}`), fakeHash("d43"), nowRFC3339())
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO source_metadata_heads(source_sha256,generation_id,published_at)
		VALUES(?,?,?)`, node.BlobHash, "80000000-0000-4000-8000-000000000008", nowRFC3339())
	require.NoError(t, err)

	coverage, err := s.DocumentEventCoverageReport(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), coverage.MissingMetadata,
		"a head for another contract is not an active F10 generation")
}

func coverageDocumentEventRecord(
	t *testing.T, vaultID, versionID, digestSeed string, dateKind document.DateKind, invalid bool,
) document.DocumentEventsV1 {
	t.Helper()
	record := documentEventRecord(t, vaultID, versionID, digestSeed)
	event := &record.Events[0]
	event.DateKind = dateKind
	event.EventID = document.DocumentEventID(vaultID, versionID, event.SourceKey, event.DateKind,
		event.DateValue, event.Precision, event.TimezoneKind, event.EvidenceSHA256)
	record.Primaries = []document.DocumentEventPrimaryV1{
		{EventID: event.EventID, Reason: "synthetic", RuleID: "primary-rule/v1", ScopeClass: "vault", Disclosure: "full"},
		{EventID: event.EventID, Reason: "synthetic", RuleID: "primary-rule/v1", ScopeClass: "vault", Disclosure: "safe"},
	}
	if invalid {
		record.Diagnostics = []document.DocumentEventDiagnosticV1{{
			Code: "date_unparseable", Detail: "safe synthetic diagnostic", SourceKey: "metadata/safe/created",
		}}
	}
	return record
}
