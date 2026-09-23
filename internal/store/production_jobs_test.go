package store

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	productionservice "go.kenn.io/docbank/internal/production"
)

func productionHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func productionJobFixture(t *testing.T) (*Store, productionservice.JobRequest) {
	t.Helper()
	s := newTestStore(t)
	set, draft, err := s.CreateProductionSet(t.Context(), "test-agent", redaction.CreateRequest{
		OperationID: "11111111-1111-4111-8111-111111111111", Name: "Synthetic job fixture",
		Instructions: "Keep synthetic material.",
	})
	require.NoError(t, err)
	setID, jobID, opID := set.ID, "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	prepared := productionHash("prepared")
	_, err = s.db.Exec(`INSERT INTO production_finalized_revisions(set_id,revision,etag,draft_sha256,prepared_sha256,prepared_input_sha256,draft_json,prepared_input_json,numbering_namespace_id,numbering_snapshot_id,numbering_recipe_sha256,numbering_start_at,finalized_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, setID, draft.Revision, draft.ETag, productionHash("draft"), prepared, prepared, []byte(`{}`), []byte(`{}`), "44444444-4444-4444-8444-444444444444", "55555555-5555-4555-8555-555555555555", productionHash("recipe"), 0, nowRFC3339())
	require.NoError(t, err)
	return s, productionservice.JobRequest{JobID: jobID, OperationID: opID, SetID: setID, Revision: draft.Revision, ETag: draft.ETag, PreparedInputSHA256: prepared, RevisionSHA256: prepared, NumberingProfileSHA256: productionHash("layout")}
}

func TestProductionJobStoreFencesClaimsAndPreservesStagedArtifacts(t *testing.T) {
	s, req := productionJobFixture(t)
	job, err := s.AdmitProductionJob(t.Context(), req)
	require.NoError(t, err)
	first, err := s.ClaimProductionJob(t.Context(), job.ID, "worker-a", time.Hour)
	require.NoError(t, err)
	_, err = s.ClaimProductionJob(t.Context(), job.ID, "worker-b", time.Hour)
	require.ErrorIs(t, err, productionservice.ErrJobStaleClaim)
	_, err = s.db.Exec(`UPDATE production_jobs SET lease_expires_at=? WHERE job_id=?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), job.ID)
	require.NoError(t, err)
	second, err := s.ClaimProductionJob(t.Context(), job.ID, "worker-b", time.Hour)
	require.NoError(t, err)
	require.Greater(t, second.Epoch, first.Epoch)
	a := documentproduction.Artifact{ID: "66666666-6666-4666-8666-666666666666", MemberID: "99999999-9999-4999-8999-999999999999", MemberOrdinal: 1, Page: 1, Role: documentproduction.ArtifactRoleRedactedPage, Path: "VOL001/page.png", SHA256: productionHash("artifact"), Size: 3, MediaType: "image/png"}
	require.NoError(t, s.StageProductionArtifact(t.Context(), second, a))
	a.Path = "VOL001/changed.png"
	require.ErrorIs(t, s.StageProductionArtifact(t.Context(), second, a), productionservice.ErrJobConflict)
}

func TestProductionJobStoreRenewsExpiredRenderClaim(t *testing.T) {
	s, req := productionJobFixture(t)
	job, err := s.AdmitProductionJob(t.Context(), req)
	require.NoError(t, err)
	claim, err := s.ClaimProductionJob(t.Context(), job.ID, "renderer", time.Millisecond)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	renewed, err := s.RenewProductionJobClaim(t.Context(), claim, time.Minute)
	require.NoError(t, err)
	require.True(t, renewed.ExpiresAt.After(time.Now()))
	_, err = s.ClaimProductionJob(t.Context(), job.ID, "other", time.Minute)
	require.ErrorIs(t, err, productionservice.ErrJobStaleClaim)
}

func TestProductionJobStoreRenewalRejectsCanceledAndRotatedClaims(t *testing.T) {
	s, req := productionJobFixture(t)
	job, err := s.AdmitProductionJob(t.Context(), req)
	require.NoError(t, err)
	claim, err := s.ClaimProductionJob(t.Context(), job.ID, "renderer", time.Minute)
	require.NoError(t, err)
	require.NoError(t, s.CancelProductionJob(t.Context(), job.ID))
	_, err = s.RenewProductionJobClaim(t.Context(), claim, time.Minute)
	require.ErrorIs(t, err, productionservice.ErrJobStaleClaim)
}

func TestProductionJobStorePublicationRequiresStagedManifestAndLiveLease(t *testing.T) {
	s, req := productionJobFixture(t)
	job, err := s.AdmitProductionJob(t.Context(), req)
	require.NoError(t, err)
	claim, err := s.ClaimProductionJob(t.Context(), job.ID, "worker", time.Hour)
	require.NoError(t, err)
	a := documentproduction.Artifact{ID: "77777777-7777-4777-8777-777777777777", MemberID: "99999999-9999-4999-8999-999999999999", MemberOrdinal: 1, Page: 1, Role: documentproduction.ArtifactRoleRedactedPage, Path: "VOL001/page.png", SHA256: productionHash("artifact"), Size: 3, MediaType: "image/png"}
	require.NoError(t, s.StageProductionArtifact(t.Context(), claim, a))
	manifest := documentproduction.ArtifactManifest{Contract: documentproduction.ArtifactManifestContractV1, Artifacts: []documentproduction.Artifact{a}}
	_, manifest.SHA256, err = documentproduction.CanonicalArtifactManifest(manifest)
	require.NoError(t, err)
	reservation := documentproduction.NumberReservation{Contract: documentproduction.NumberReservationContractV1, Authority: "bates-ledger/v1", ID: "88888888-8888-4888-8888-888888888888", OperationID: job.ID, RevisionSHA256: req.PreparedInputSHA256, State: "reserved", Numbers: []documentproduction.AssignedNumber{{MemberID: "99999999-9999-4999-8999-999999999999", MemberOrdinal: 1, Page: 1, Text: "B000001"}}}
	_, reservation.SHA256, err = documentproduction.CanonicalNumberReservation(reservation)
	require.NoError(t, err)
	job.Reservation = reservation
	endorsementBytes, err := canonical.Marshal([]any{})
	require.NoError(t, err)
	endorsementHash := sha256.Sum256(endorsementBytes)
	receipt := documentproduction.ProductionReceipt{Contract: documentproduction.ProductionReceiptContractV1, ID: job.ID, JobID: job.ID, SetID: req.SetID, Revision: req.Revision, RevisionSHA256: req.RevisionSHA256, PreparedInputSHA256: req.PreparedInputSHA256, PolicySHA256: productionHash("policy"), NumberReservationSHA256: reservation.SHA256, LayoutSHA256: req.NumberingProfileSHA256, EndorsementsSHA256: hex.EncodeToString(endorsementHash[:]), ArtifactManifestSHA256: manifest.SHA256, CreatedAt: nowRFC3339()}
	_, receipt.SHA256, err = documentproduction.CanonicalProductionReceipt(receipt)
	require.NoError(t, err)
	wrongManifest := manifest
	wrongManifest.Artifacts = append([]documentproduction.Artifact(nil), manifest.Artifacts...)
	wrongManifest.Artifacts[0].Path = "VOL001/other.png"
	_, wrongManifest.SHA256, err = documentproduction.CanonicalArtifactManifest(wrongManifest)
	require.NoError(t, err)
	wrongReceipt := receipt
	wrongReceipt.ArtifactManifestSHA256 = wrongManifest.SHA256
	_, wrongReceipt.SHA256, err = documentproduction.CanonicalProductionReceipt(wrongReceipt)
	require.NoError(t, err)
	_, err = s.PublishProductionJob(t.Context(), claim, job, wrongReceipt, wrongManifest, nil)
	require.ErrorIs(t, err, productionservice.ErrJobIncomplete)
	_, err = s.db.Exec(`UPDATE production_jobs SET lease_expires_at=? WHERE job_id=?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), job.ID)
	require.NoError(t, err)
	_, err = s.PublishProductionJob(t.Context(), claim, job, receipt, manifest, nil)
	require.ErrorIs(t, err, productionservice.ErrJobStaleClaim)
}
