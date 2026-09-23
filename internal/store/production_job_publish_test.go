package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/production"
)

func stagedProductionPublicationFixture(t *testing.T) (*Store, production.JobClaim, production.Job,
	documentproduction.ProductionReceipt, documentproduction.ArtifactManifest, []redaction.Endorsement) {
	t.Helper()
	s, claim, _ := productionPageStageFixture(t)
	job, err := s.LoadProductionJob(t.Context(), claim.JobID)
	require.NoError(t, err)
	plan, err := s.LoadProductionRenderPlan(t.Context(), job.ID)
	require.NoError(t, err)
	job.Reservation = plan.Reservation
	finalized, err := s.LoadFinalizedProduction(t.Context(), job.SetID, job.Revision)
	require.NoError(t, err)
	var artifacts []documentproduction.Artifact
	for _, page := range plan.Pages {
		var member documentproduction.PreparedMember
		for _, candidate := range finalized.Authority.Prepared.Members {
			if candidate.Member.ID == page.MemberID {
				member = candidate
				break
			}
		}
		require.NotEmpty(t, member.Member.ID)
		id := uuid.NewString()
		artifact := documentproduction.Artifact{ID: id, MemberID: page.MemberID, MemberOrdinal: page.MemberOrdinal,
			Page: page.Page, Role: documentproduction.ArtifactRoleRedactedPage, Path: "VOL001/" + id + ".png",
			SHA256: productionHash(fmt.Sprintf("synthetic page %s %d", page.MemberID, page.Page)), Size: 13,
			MediaType: "image/png", Volume: "VOL001"}
		require.NoError(t, s.RecordBlob(t.Context(), artifact.SHA256, artifact.Size,
			BlobPhysical{Encoding: "raw", StoredBytes: artifact.Size, Created: true}))
		stage, err := production.BuildProductionPageStage(job, plan, member, page, artifact)
		require.NoError(t, err)
		_, err = s.StageProductionPage(t.Context(), claim, stage)
		require.NoError(t, err)
		artifacts = append(artifacts, artifact)
	}
	for _, member := range finalized.Authority.Prepared.Members {
		id := uuid.NewString()
		artifact := documentproduction.Artifact{ID: id, MemberID: member.Member.ID,
			MemberOrdinal: member.Member.Ordinal, Role: documentproduction.ArtifactRoleRedactedPDF,
			Path: "VOL001/" + id + ".pdf", SHA256: productionHash("synthetic final PDF " + member.Member.ID),
			Size: 23, MediaType: "application/pdf", Volume: "VOL001"}
		require.NoError(t, s.RecordBlob(t.Context(), artifact.SHA256, artifact.Size,
			BlobPhysical{Encoding: "raw", StoredBytes: artifact.Size, Created: true}))
		require.NoError(t, s.StageProductionArtifact(t.Context(), claim, artifact))
		artifacts = append(artifacts, artifact)
	}
	manifest := documentproduction.ArtifactManifest{Contract: documentproduction.ArtifactManifestContractV1, Artifacts: artifacts}
	_, manifest.SHA256, err = documentproduction.CanonicalArtifactManifest(manifest)
	require.NoError(t, err)
	endorsements := make([]redaction.Endorsement, 0)
	for _, page := range plan.Pages {
		endorsements = append(endorsements, page.Endorsements...)
	}
	receipt := documentproduction.ProductionReceipt{Contract: documentproduction.ProductionReceiptContractV1,
		ID: job.ID, JobID: job.ID, SetID: job.SetID, Revision: job.Revision,
		RevisionSHA256: job.RevisionSHA256, PreparedInputSHA256: job.PreparedInputSHA256,
		PolicySHA256:            finalized.Authority.Prepared.Policy.SHA256,
		NumberReservationSHA256: plan.Reservation.SHA256, LayoutSHA256: plan.LayoutSHA256,
		EndorsementsSHA256:     productionEndorsementDigest(endorsements),
		ArtifactManifestSHA256: manifest.SHA256, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	_, receipt.SHA256, err = documentproduction.CanonicalProductionReceipt(receipt)
	require.NoError(t, err)
	return s, claim, job, receipt, manifest, endorsements
}

func TestProductionJobPublishesOnlyCompleteStageBoundArtifacts(t *testing.T) {
	s, claim, job, receipt, manifest, endorsements := stagedProductionPublicationFixture(t)
	missing := manifest
	missing.Artifacts = append([]documentproduction.Artifact(nil), manifest.Artifacts[:len(manifest.Artifacts)-1]...)
	_, missing.SHA256, _ = documentproduction.CanonicalArtifactManifest(missing)
	wrong := receipt
	wrong.ArtifactManifestSHA256 = missing.SHA256
	_, wrong.SHA256, _ = documentproduction.CanonicalProductionReceipt(wrong)
	_, err := s.PublishProductionJob(t.Context(), claim, job, wrong, missing, endorsements)
	require.ErrorIs(t, err, production.ErrJobIncomplete)
	changedPlan := receipt
	changedPlan.LayoutSHA256 = productionHash("changed layout")
	_, changedPlan.SHA256, err = documentproduction.CanonicalProductionReceipt(changedPlan)
	require.NoError(t, err)
	_, err = s.PublishProductionJob(t.Context(), claim, job, changedPlan, manifest, endorsements)
	require.ErrorIs(t, err, production.ErrJobConflict)
	result, err := s.PublishProductionJob(t.Context(), claim, job, receipt, manifest, endorsements)
	require.NoError(t, err)
	require.Equal(t, production.ProductionJobSucceeded, result.State)
	// A restart after a lost response can recover the exact immutable receipt.
	reopened, err := Open(s.path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	loaded, err := reopened.LoadProductionJob(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, receipt, loaded.Receipt)
	require.Equal(t, manifest, loaded.Manifest)
	for _, artifact := range manifest.Artifacts {
		got, err := reopened.LoadProductionJobArtifact(t.Context(), job.ID, artifact.ID)
		require.NoError(t, err)
		require.Equal(t, artifact, got)
	}
}

func TestProductionJobPublicationRejectsCanceledOrStaleClaim(t *testing.T) {
	for _, mode := range []string{"stale", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			s, claim, job, receipt, manifest, endorsements := stagedProductionPublicationFixture(t)
			if mode == "stale" {
				claim.Epoch++
			} else {
				require.NoError(t, s.CancelProductionJob(t.Context(), job.ID))
			}
			_, err := s.PublishProductionJob(t.Context(), claim, job, receipt, manifest, endorsements)
			require.ErrorIs(t, err, production.ErrJobStaleClaim)
			loaded, err := s.LoadProductionJob(t.Context(), job.ID)
			require.NoError(t, err)
			require.Empty(t, loaded.Receipt.SHA256)
		})
	}
}

func TestProductionFinalArtifactStageRejectsExpiredClaim(t *testing.T) {
	s, claim, job, _, manifest, _ := stagedProductionPublicationFixture(t)
	_, err := s.db.Exec(`UPDATE production_jobs SET lease_expires_at=? WHERE job_id=?`,
		"2020-01-01T00:00:00Z", job.ID)
	require.NoError(t, err)
	artifact := manifest.Artifacts[len(manifest.Artifacts)-1]
	artifact.ID = uuid.NewString()
	artifact.Path = "VOL001/expired.pdf"
	err = s.StageProductionArtifact(t.Context(), claim, artifact)
	require.ErrorIs(t, err, production.ErrJobStaleClaim)
	_, err = s.LoadProductionJobArtifact(t.Context(), job.ID, artifact.ID)
	require.Error(t, err)
}

func TestProductionJobLoadRejectsOversizedStoredReceiptBeforeDecode(t *testing.T) {
	s, request := productionJobFixture(t)
	_, err := s.AdmitProductionJob(t.Context(), request)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE production_jobs SET receipt_json=zeroblob(?) WHERE job_id=?`,
		maxProductionReceiptBytes+1, request.JobID)
	require.NoError(t, err)
	_, err = s.LoadProductionJob(t.Context(), request.JobID)
	require.ErrorIs(t, err, production.ErrJobConflict)
}
