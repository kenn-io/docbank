package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/kit/packstore"
)

type productionPDFReader struct {
	*bytes.Reader

	verified bool
}

func (r *productionPDFReader) Close() error { return nil }
func (r *productionPDFReader) Verify() error {
	if !r.verified {
		return errors.New("synthetic corrupt blob")
	}
	return nil
}
func (r *productionPDFReader) Verified() bool { return r.verified }

type productionPDFOpener struct {
	stream packstore.VerifiedReadCloser
	size   int64
	err    error
	opened string
}

func (o *productionPDFOpener) OpenStreamContext(_ context.Context, hash string) (packstore.VerifiedReadCloser, int64, error) {
	o.opened = hash
	return o.stream, o.size, o.err
}

func finalizedProductionCheckpointFixture(t *testing.T) (*Store, production.FinalizedProduction, production.Job, documentproduction.NumberReservation) {
	t.Helper()
	s, _, _, setID, revision, authority := productionDuplicateGateFixture(t)
	const snapshotID = "76000000-0000-4000-8000-000000000001"
	_, err := s.SealProductionNumberingSnapshot(t.Context(), snapshotID, authority.Audit.OperationID)
	require.NoError(t, err)
	namespace, err := s.EnsureBatesNamespace(t.Context(), "PLAN", "", 6)
	require.NoError(t, err)
	draft, err := s.ProductionDraft(t.Context(), setID, revision)
	require.NoError(t, err)
	draft.State = "finalized"
	finalized := production.FinalizedProduction{Draft: draft, Authority: authority}
	require.NoError(t, s.FinalizeProductionRevision(t.Context(), production.FinalizationRequest{
		Finalized: finalized, OperationID: authority.Audit.OperationID,
		NamespaceID: namespace.NamespaceID, SnapshotID: snapshotID, RecipeSHA256: draft.NumberingRecipeSHA256,
	}))
	job, err := s.AdmitProductionJob(t.Context(), production.JobRequest{
		JobID: "76000000-0000-4000-8000-000000000002", OperationID: "76000000-0000-4000-8000-000000000003",
		SetID: setID, Revision: revision, ETag: draft.ETag,
		PreparedInputSHA256: authority.Receipt.SHA256, RevisionSHA256: authority.Prepared.SHA256,
		NumberingProfileSHA256: draft.NumberingRecipeSHA256,
	})
	require.NoError(t, err)
	reservation, err := s.ReserveProductionJobNumbers(t.Context(), job, finalized)
	require.NoError(t, err)
	return s, finalized, job, reservation
}

func TestProductionFinalizedPDFHandleChecksCatalogAndVerifiedBytes(t *testing.T) {
	s, finalized, _, _ := finalizedProductionCheckpointFixture(t)
	member := finalized.Authority.Prepared.Members[0].Member
	data := []byte("synthetic derived PDF")
	opener := &productionPDFOpener{stream: &productionPDFReader{Reader: bytes.NewReader(data), verified: true}, size: int64(len(data))}
	handle, err := s.OpenFinalizedProductionPDF(t.Context(), finalized.Draft.SetID, finalized.Draft.Revision, member.ID, opener)
	require.NoError(t, err)
	require.Equal(t, member.PDFSHA256, opener.opened)
	require.Equal(t, member.SourceVersionID, handle.SourceVersionID)
	require.Equal(t, member.PDFSize, handle.Size)
	_, err = io.Copy(io.Discard, handle.Stream)
	require.NoError(t, err)
	require.NoError(t, handle.Stream.Verify())
	require.True(t, handle.Stream.Verified())
	require.NoError(t, handle.Stream.Close())

	missing := &productionPDFOpener{err: errors.New("missing synthetic blob")}
	_, err = s.OpenFinalizedProductionPDF(t.Context(), finalized.Draft.SetID, finalized.Draft.Revision, member.ID, missing)
	require.Error(t, err)
	corrupt := &productionPDFOpener{stream: &productionPDFReader{Reader: bytes.NewReader(data)}, size: int64(len(data))}
	handle, err = s.OpenFinalizedProductionPDF(t.Context(), finalized.Draft.SetID, finalized.Draft.Revision, member.ID, corrupt)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, handle.Stream)
	require.NoError(t, err)
	require.Error(t, handle.Stream.Verify())
	require.False(t, handle.Stream.Verified())
	require.NoError(t, handle.Stream.Close())
	wrongSize := &productionPDFOpener{stream: &productionPDFReader{Reader: bytes.NewReader(data), verified: true}, size: int64(len(data)) + 1}
	_, err = s.OpenFinalizedProductionPDF(t.Context(), finalized.Draft.SetID, finalized.Draft.Revision, member.ID, wrongSize)
	require.ErrorIs(t, err, ErrInvalidProduction)
}

func TestProductionFinalizedPDFHandleRejectsWrongRenditionAndChangedHead(t *testing.T) {
	for _, change := range []string{"rendition", "source_head"} {
		t.Run(change, func(t *testing.T) {
			s, finalized, _, _ := finalizedProductionCheckpointFixture(t)
			member := finalized.Authority.Prepared.Members[0].Member
			if change == "rendition" {
				// Simulate a damaged catalog row; ordinary writes are stopped by
				// immutable triggers and foreign keys.
				_, err := s.db.Exec(`DROP TRIGGER production_text_maps_immutable_update`)
				require.NoError(t, err)
				conn, err := s.db.Conn(t.Context())
				require.NoError(t, err)
				_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF`)
				require.NoError(t, err)
				_, err = conn.ExecContext(t.Context(), `UPDATE production_text_maps SET rendition_artifact_id='wrong-synthetic-artifact' WHERE map_sha256=?`, member.MapSHA256)
				require.NoError(t, err)
				_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=ON`)
				require.NoError(t, err)
				require.NoError(t, conn.Close())
			} else {
				require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
					const newVersion = "76000000-0000-4000-8000-000000000004"
					_, err := tx.ExecContext(t.Context(), `INSERT INTO content_versions(version_id,node_id,blob_hash,size,recorded_at,node_revision,introduced_operation_id,transition_kind)
						SELECT ?,node_id,blob_hash,size,?,node_revision+1,?,'content_replace' FROM content_versions WHERE version_id=?`,
						newVersion, nowRFC3339(), "76000000-0000-4000-8000-000000000005", member.SourceVersionID)
					if err != nil {
						return err
					}
					_, err = tx.ExecContext(t.Context(), `UPDATE nodes SET current_version_id=?,revision=revision+1 WHERE id=?`, newVersion, member.NodeID)
					return err
				}))
			}
			_, err := s.LoadFinalizedProduction(t.Context(), finalized.Draft.SetID, finalized.Draft.Revision)
			require.ErrorIs(t, err, ErrInvalidProduction)
			opener := &productionPDFOpener{stream: &productionPDFReader{Reader: bytes.NewReader([]byte("synthetic derived PDF")), verified: true}, size: member.PDFSize}
			_, err = s.OpenFinalizedProductionPDF(t.Context(), finalized.Draft.SetID, finalized.Draft.Revision, member.ID, opener)
			require.ErrorIs(t, err, ErrInvalidProduction)
			require.Empty(t, opener.opened, "catalog rejection precedes physical blob access")
		})
	}
}

func TestProductionRenderPlanCheckpointSurvivesReplayAndFencesClaim(t *testing.T) {
	s, finalized, job, reservation := finalizedProductionCheckpointFixture(t)
	plan := production.RenderPlan{Contract: production.RenderPlanContractV1, JobID: job.ID, RevisionSHA256: job.RevisionSHA256, Reservation: reservation}
	recipe, err := pdfproduction.QualifiedRecipeForDPI(300)
	require.NoError(t, err)
	for _, prepared := range finalized.Authority.Prepared.Members {
		endorsed, planErr := production.PlanEndorsementPages(prepared.Member.ID, prepared.Resolved.Pages, prepared.Resolved, reservation.Numbers, recipe)
		require.NoError(t, planErr)
		for index, page := range prepared.Resolved.Pages {
			plan.Pages = append(plan.Pages, production.RenderPagePlan{MemberID: prepared.Member.ID, MemberOrdinal: prepared.Member.Ordinal,
				Page: page.Number, ResolvedSHA256: prepared.Resolved.SHA256, Layout: endorsed[index].Layout,
				Endorsements: endorsed[index].Endorsements})
		}
	}
	require.NotEmpty(t, plan.Pages[0].Endorsements)
	unsealed := plan
	unsealed.Pages = append([]production.RenderPagePlan(nil), plan.Pages...)
	unsealed.Pages[0].Endorsements = append([]redaction.Endorsement(nil), plan.Pages[0].Endorsements...)
	unsealed.Pages[0].Endorsements[0].Text = "PRIVATE-SYNTHETIC-REASON"
	_, err = s.CheckpointProductionRenderPlan(t.Context(), job, unsealed)
	require.ErrorIs(t, err, production.ErrJobConflict)
	_, err = s.ClaimProductionRenderStage(t.Context(), job.ID, "renderer", time.Minute)
	require.ErrorIs(t, err, production.ErrJobIncomplete)
	stored, err := s.CheckpointProductionRenderPlan(t.Context(), job, plan)
	require.NoError(t, err)
	require.NotEmpty(t, stored.SHA256)
	loaded, err := s.LoadProductionRenderPlan(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, stored, loaded)
	replayed, err := s.CheckpointProductionRenderPlan(t.Context(), job, plan)
	require.NoError(t, err)
	require.Equal(t, stored, replayed)
	// The first acknowledgement may be lost. Reopen the database and
	// recover the exact plan without reserving another Bates range.
	reopened, err := Open(s.path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	recovered, err := reopened.LoadProductionRenderPlan(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, stored, recovered)
	replayed, err = reopened.CheckpointProductionRenderPlan(t.Context(), job, plan)
	require.NoError(t, err)
	require.Equal(t, stored, replayed)
	var allocations int
	require.NoError(t, reopened.db.QueryRow(`SELECT COUNT(*) FROM bates_allocations WHERE operation_id=?`, job.ID).Scan(&allocations))
	require.Equal(t, 1, allocations)
	changed := plan
	changed.Pages = append([]production.RenderPagePlan(nil), plan.Pages...)
	changed.Pages[0].Layout.StripHeight++
	_, err = s.CheckpointProductionRenderPlan(t.Context(), job, changed)
	require.ErrorIs(t, err, production.ErrJobConflict)
	_, err = s.ClaimProductionRenderStage(t.Context(), job.ID, "renderer", time.Minute)
	require.NoError(t, err)
}
