package store

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
)

// A lost response must not cause a source resolver to select a newer head.
func TestExportSourceRetryFreezesHistoricalMembershipAndProtectsPrune(t *testing.T) {
	s := newTestStore(t)
	n, err := s.CreateFile(t.Context(), s.RootID(), "synthetic.txt", fakeHash("a1"), 12, "text/plain")
	require.NoError(t, err)
	request := bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "explicit", Members: []bundle.Member{{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size}}}
	calls := 0
	resolve := func(context.Context) ([]bundle.Member, error) { calls++; return request.Members, nil }
	first, err := s.CreateExportSource(t.Context(), "owner", request, resolve)
	require.NoError(t, err)
	next, _, err := s.ReplaceContent(t.Context(), n.ID, n.Revision, fakeHash("a2"), 13, "text/plain")
	require.NoError(t, err)
	retry, err := s.CreateExportSource(t.Context(), "owner", request, resolve)
	require.NoError(t, err)
	require.Equal(t, first, retry)
	require.Equal(t, 1, calls)
	_, err = s.PruneContentVersions(t.Context(), n.ID, next.Revision, VersionPruneSelector{AllPrior: true}, true)
	require.ErrorIs(t, err, bundle.ErrRetained)
	changed := request
	changed.Members = []bundle.Member{{NodeID: n.ID, VersionID: next.CurrentVersionID, SHA256: next.BlobHash, Size: next.Size}}
	_, err = s.CreateExportSource(t.Context(), "owner", changed, resolve)
	require.ErrorIs(t, err, bundle.ErrConflict)
	_, err = s.ExportSource(t.Context(), "other", first.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestExportJobsFenceRestartCancelAndPortableAuthority(t *testing.T) {
	s := newTestStore(t)
	n, err := s.CreateFile(t.Context(), s.RootID(), "synthetic.txt", fakeHash("d1"), 12, "text/plain")
	require.NoError(t, err)
	source, err := s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "explicit", Members: []bundle.Member{{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size}}}, nil)
	require.NoError(t, err)
	plan, err := s.CreateExportPlan(t.Context(), "owner", bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}}})
	require.NoError(t, err)
	job, err := s.QueueExportJob(t.Context(), "owner", bundle.JobRequest{OperationID: uuid.NewString(), PlanID: plan.ID, Fingerprint: plan.Fingerprint})
	require.NoError(t, err)
	claim, err := s.ClaimExportJob(t.Context())
	require.NoError(t, err)
	require.NoError(t, s.RequeueExportJobs(t.Context()))
	require.ErrorIs(t, s.CheckExportClaim(t.Context(), claim), bundle.ErrFenced)
	current, err := s.ClaimExportJob(t.Context())
	require.NoError(t, err)
	require.Greater(t, current.Epoch, claim.Epoch)
	require.NoError(t, s.CancelExportJob(t.Context(), "owner", job.ID))
	require.ErrorIs(t, s.CheckExportClaim(t.Context(), current), bundle.ErrFenced)
	var metadata bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &metadata))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(metadata.Bytes())))
	_, err = restored.ExportPlan(t.Context(), "owner", plan.ID)
	require.ErrorIs(t, err, ErrNotFound)
	var docs []bundle.Document
	require.NoError(t, restored.WalkExportDocuments(t.Context(), plan.ID, func(d bundle.Document) error { docs = append(docs, d); return nil }))
	require.Len(t, docs, 1)
	// Cancellation drops the two-hour job extension. After the admission
	// window and terminal record expire, exact prune protection is released.
	_, err = s.db.ExecContext(t.Context(), `UPDATE export_jobs SET expires_at='2000-01-01T00:00:00.000000000Z' WHERE id=?`, job.ID)
	require.NoError(t, err)
	expired, err := s.ExpiredExportJobs(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{job.ID}, expired)
	require.NoError(t, s.DeleteExpiredExportJob(t.Context(), job.ID))
	_, err = s.db.ExecContext(t.Context(), `UPDATE export_plans SET expires_at='2000-01-01T00:00:00.000000000Z' WHERE id=?`, plan.ID)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `UPDATE export_sources SET expires_at='2000-01-01T00:00:00.000000000Z' WHERE id=?`, source.ID)
	require.NoError(t, err)
	require.NoError(t, s.CleanupExportAuthority(t.Context()))
	next, _, err := s.ReplaceContent(t.Context(), n.ID, n.Revision, fakeHash("d2"), 13, "text/plain")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(t.Context(), n.ID, next.Revision, VersionPruneSelector{AllPrior: true}, true)
	require.NoError(t, err)
}

func TestExportAdmissionRejectsOversizedUploadBeforeResolution(t *testing.T) {
	s := newTestStore(t)
	called := false
	_, err := s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "upload", Total: bundle.MaxMembers + 1, MemberHash: fakeHash("aa")}, func(context.Context) ([]bundle.Member, error) {
		called = true
		return nil, nil
	})
	require.ErrorIs(t, err, bundle.ErrLimit)
	require.False(t, called)
	var count int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM export_sources`).Scan(&count))
	require.Zero(t, count)
}

func TestExportChunksSealExactVersionsAndRejectChangedRetries(t *testing.T) {
	s := newTestStore(t)
	n, err := s.CreateFile(t.Context(), s.RootID(), "synthetic.txt", fakeHash("b1"), 1, "text/plain")
	require.NoError(t, err)
	old := bundle.Member{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size}
	next, _, err := s.ReplaceContent(t.Context(), n.ID, n.Revision, fakeHash("b2"), 2, "text/plain")
	require.NoError(t, err)
	members := []bundle.Member{old, {NodeID: n.ID, VersionID: next.CurrentVersionID, SHA256: next.BlobHash, Size: next.Size}}
	source, err := s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "upload", Total: 2, MemberHash: ExportMemberHash(members)}, nil)
	require.NoError(t, err)
	require.NoError(t, s.PutExportChunk(t.Context(), "owner", source.ID, 0, members))
	require.NoError(t, s.PutExportChunk(t.Context(), "owner", source.ID, 0, members))
	require.ErrorIs(t, s.PutExportChunk(t.Context(), "owner", source.ID, 0, []bundle.Member{old}), bundle.ErrConflict)
	sealed, err := s.SealExportSource(t.Context(), "owner", source.ID)
	require.NoError(t, err)
	require.Equal(t, 2, sealed.Total)
	require.Equal(t, "sealed", sealed.State)
}

func TestExportPlanPinsRolesAndCannotInventMissingText(t *testing.T) {
	s := newTestStore(t)
	n, err := s.CreateFile(t.Context(), s.RootID(), "synthetic.txt", fakeHash("c1"), 12, "text/plain")
	require.NoError(t, err)
	source, err := s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "explicit", Members: []bundle.Member{{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size}}}, nil)
	require.NoError(t, err)
	_, err = s.CreateExportPlan(t.Context(), "owner", bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "text"}}})
	require.ErrorIs(t, err, bundle.ErrUnavailable)
	request := bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}, {Role: "text", AllowUnavailable: true}}}
	plan, err := s.CreateExportPlan(t.Context(), "owner", request)
	require.NoError(t, err)
	require.NotEmpty(t, plan.Fingerprint)
	_, _, err = s.ReplaceContent(t.Context(), n.ID, n.Revision, fakeHash("c2"), 13, "text/plain")
	require.NoError(t, err)
	retry, err := s.CreateExportPlan(t.Context(), "owner", request)
	require.NoError(t, err)
	require.Equal(t, plan, retry)
	var documents []bundle.Document
	err = s.WalkExportDocuments(t.Context(), plan.ID, func(d bundle.Document) error { documents = append(documents, d); return nil })
	require.NoError(t, err)
	require.Len(t, documents, 1)
	require.Equal(t, n.CurrentVersionID, documents[0].VersionID)
	require.Equal(t, "unavailable", documents[0].Roles[1].Status)
	require.Empty(t, documents[0].Roles[1].Path)
}
