package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/production"
)

func ProductionFinalizedJobHTTPFixture(t *testing.T) (*Store, string, string, int64, int64, redaction.Member) {
	t.Helper()
	s, first, _, setID, revision, authority := productionDuplicateGateFixture(t)
	const snapshotID = "77000000-0000-4000-8000-000000000020"
	_, err := s.SealProductionNumberingSnapshot(t.Context(), snapshotID, authority.Audit.OperationID)
	require.NoError(t, err)
	namespace, err := s.EnsureBatesNamespace(t.Context(), "PROD", "", 6)
	require.NoError(t, err)
	draft, err := s.ProductionDraft(t.Context(), setID, revision)
	require.NoError(t, err)
	draft.State = "finalized"
	require.NoError(t, s.FinalizeProductionRevision(t.Context(), production.FinalizationRequest{
		Finalized:   production.FinalizedProduction{Draft: draft, Authority: authority},
		OperationID: authority.Audit.OperationID, NamespaceID: namespace.NamespaceID,
		SnapshotID: snapshotID, RecipeSHA256: draft.NumberingRecipeSHA256,
	}))
	return s, filepath.Dir(s.path), setID, revision, draft.ETag, first
}

func TestAdmitFinalizedProductionJobDerivesAuthorityAndReplays(t *testing.T) {
	s, _, setID, revision, etag, first := ProductionFinalizedJobHTTPFixture(t)
	finalized, err := s.LoadFinalizedProduction(t.Context(), setID, revision)
	require.NoError(t, err)
	const jobID = "77000000-0000-4000-8000-000000000021"
	const operationID = "77000000-0000-4000-8000-000000000022"
	job, err := s.AdmitFinalizedProductionJob(t.Context(), setID, revision, etag, jobID, operationID)
	require.NoError(t, err)
	require.Equal(t, production.ProductionJobQueued, job.State)
	require.Equal(t, finalized.Authority.Receipt.SHA256, job.PreparedInputSHA256)
	require.Equal(t, finalized.Authority.Prepared.SHA256, job.RevisionSHA256)
	replay, err := s.AdmitFinalizedProductionJob(t.Context(), setID, revision, etag, jobID, operationID)
	require.NoError(t, err)
	require.Equal(t, job, replay)
	const newVersion = "77000000-0000-4000-8000-000000000026"
	_, err = s.db.Exec(`INSERT INTO content_versions(version_id,node_id,blob_hash,size,recorded_at,node_revision,introduced_operation_id,transition_kind)
		SELECT ?,node_id,blob_hash,size,?,node_revision+1,?,'content_replace' FROM content_versions WHERE version_id=?`,
		newVersion, nowRFC3339(), "77000000-0000-4000-8000-000000000027", first.SourceVersionID)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE nodes SET current_version_id=?,revision=revision+1 WHERE id=?`, newVersion, first.NodeID)
	require.NoError(t, err)
	replay, err = s.AdmitFinalizedProductionJob(t.Context(), setID, revision, etag, jobID, operationID)
	require.NoError(t, err, "an admitted job replays after source-head drift")
	require.Equal(t, job, replay)
	_, err = s.AdmitFinalizedProductionJob(t.Context(), setID, revision, etag+1, jobID, operationID)
	require.ErrorIs(t, err, ErrProductionRevisionConflict)
	_, err = s.AdmitFinalizedProductionJob(t.Context(), setID, revision, etag,
		"77000000-0000-4000-8000-000000000023", operationID)
	require.ErrorIs(t, err, production.ErrJobConflict)
	_, err = s.AdmitFinalizedProductionJob(t.Context(), "77000000-0000-4000-8000-000000000099",
		revision, etag, "77000000-0000-4000-8000-000000000024",
		"77000000-0000-4000-8000-000000000025")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestAdmitFinalizedProductionJobBlocksChangedSourceBeforeWrite(t *testing.T) {
	s, _, setID, revision, etag, first := ProductionFinalizedJobHTTPFixture(t)
	const newVersion = "77000000-0000-4000-8000-000000000030"
	_, err := s.db.Exec(`INSERT INTO content_versions(version_id,node_id,blob_hash,size,recorded_at,node_revision,introduced_operation_id,transition_kind)
		SELECT ?,node_id,blob_hash,size,?,node_revision+1,?,'content_replace' FROM content_versions WHERE version_id=?`,
		newVersion, nowRFC3339(), "77000000-0000-4000-8000-000000000031", first.SourceVersionID)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE nodes SET current_version_id=?,revision=revision+1 WHERE id=?`, newVersion, first.NodeID)
	require.NoError(t, err)
	const jobID = "77000000-0000-4000-8000-000000000032"
	_, err = s.AdmitFinalizedProductionJob(t.Context(), setID, revision, etag, jobID,
		"77000000-0000-4000-8000-000000000033")
	require.Error(t, err)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM production_jobs WHERE job_id=?`, jobID).Scan(&count))
	require.Zero(t, count)
}
