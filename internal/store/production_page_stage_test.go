package store

import (
	"bytes"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/production"
)

func productionPageStageFixture(t *testing.T) (*Store, production.JobClaim, production.ProductionPageStage) {
	t.Helper()
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
	plan, err = s.CheckpointProductionRenderPlan(t.Context(), job, plan)
	require.NoError(t, err)
	claim, err := s.ClaimProductionRenderStage(t.Context(), job.ID, "synthetic-renderer", time.Hour)
	require.NoError(t, err)
	page := plan.Pages[0]
	stage := production.ProductionPageStage{
		Contract: production.ProductionPageStageContractV1, JobID: job.ID, MemberID: page.MemberID, MemberOrdinal: page.MemberOrdinal,
		Page: page.Page, RevisionSHA256: job.RevisionSHA256, PreparedInputSHA256: job.PreparedInputSHA256,
		ReservationSHA256: plan.Reservation.SHA256, RenderPlanSHA256: plan.SHA256,
		ResolvedSHA256: page.ResolvedSHA256, RecipeSHA256: finalized.Authority.Prepared.Members[0].Resolved.RecipeSHA256,
		Artifact: documentproduction.Artifact{ID: "76000000-0000-4000-8000-000000000009", MemberID: page.MemberID,
			MemberOrdinal: page.MemberOrdinal, Page: page.Page, Role: documentproduction.ArtifactRoleRedactedPage,
			Path: "VOL001/synthetic-page.png", SHA256: productionHash("synthetic PNG"), Size: 13, MediaType: "image/png"},
	}
	stage.LayoutSHA256, stage.EndorsementsSHA256, err = productionPagePlanDigests(page)
	require.NoError(t, err)
	require.NoError(t, s.RecordBlob(t.Context(), stage.Artifact.SHA256, stage.Artifact.Size,
		BlobPhysical{Encoding: "raw", StoredBytes: stage.Artifact.Size, Created: true}))
	return s, claim, stage
}

func TestProductionPageStageRequiresPhysicalAuthority(t *testing.T) {
	for _, mode := range []string{"missing", "wrong_size"} {
		t.Run(mode, func(t *testing.T) {
			s, claim, stage := productionPageStageFixture(t)
			var err error
			switch mode {
			case "missing":
				_, err = s.db.Exec(`DELETE FROM blob_locations WHERE blob_hash=?`, stage.Artifact.SHA256)
			case "wrong_size":
				_, err = s.db.Exec(`UPDATE blobs SET size=size+1 WHERE hash=?`, stage.Artifact.SHA256)
			}
			require.NoError(t, err)
			_, err = s.StageProductionPage(t.Context(), claim, stage)
			require.ErrorIs(t, err, production.ErrJobConflict)
			var count int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM production_job_page_stages WHERE job_id=?`, claim.JobID).Scan(&count))
			require.Zero(t, count)
		})
	}
}

func TestProductionPageStageRootsStagedPNGForGC(t *testing.T) {
	s, claim, stage := productionPageStageFixture(t)
	unreachable, err := s.UnreachableBlobs(t.Context())
	require.NoError(t, err)
	require.True(t, slices.ContainsFunc(unreachable, func(blob BlobInfo) bool { return blob.Hash == stage.Artifact.SHA256 }))
	_, err = s.StageProductionPage(t.Context(), claim, stage)
	require.NoError(t, err)
	unreachable, err = s.UnreachableBlobs(t.Context())
	require.NoError(t, err)
	require.False(t, slices.ContainsFunc(unreachable, func(blob BlobInfo) bool { return blob.Hash == stage.Artifact.SHA256 }))
}

func TestProductionPageStageWarmCacheRejectsTamperedPlan(t *testing.T) {
	s, claim, stage := productionPageStageFixture(t)
	_, err := s.StageProductionPage(t.Context(), claim, stage)
	require.NoError(t, err)
	_, err = s.LoadProductionPageStage(t.Context(), stage.JobID, stage.MemberID, stage.Page)
	require.NoError(t, err, "warm the immutable plan proof")
	_, err = s.db.Exec(`UPDATE production_job_render_plans SET canonical_json='{}' WHERE job_id=?`, stage.JobID)
	require.Error(t, err, "ordinary mutation is blocked by the plan trigger")
	_, err = s.db.Exec(`DROP TRIGGER production_job_render_plans_immutable_update`)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE production_job_render_plans SET canonical_json='{}' WHERE job_id=?`, stage.JobID)
	require.NoError(t, err)
	_, err = s.LoadProductionPageStage(t.Context(), stage.JobID, stage.MemberID, stage.Page)
	require.ErrorIs(t, err, production.ErrJobConflict, "same-process cache must stop trusting a removed trigger")
}

func TestProductionPageStageUsesPinnedMemberRecipe(t *testing.T) {
	s, claim, stage := productionPageStageFixture(t)
	_, err := s.db.Exec(`DROP TRIGGER production_finalized_revisions_immutable_update`)
	require.NoError(t, err)
	var raw []byte
	require.NoError(t, s.db.QueryRow(`SELECT draft_json FROM production_finalized_revisions f JOIN production_jobs j
		ON j.set_id=f.set_id AND j.revision=f.revision WHERE j.job_id=?`, claim.JobID).Scan(&raw))
	draft, err := canonical.Decode[redaction.Draft](raw)
	require.NoError(t, err)
	draft.RecipeSHA256 = productionHash("not the prepared member recipe")
	raw, err = canonical.Marshal(draft)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE production_finalized_revisions SET draft_json=? WHERE set_id=(SELECT set_id FROM production_jobs WHERE job_id=?)`, raw, claim.JobID)
	require.NoError(t, err)
	stored, err := s.StageProductionPage(t.Context(), claim, stage)
	require.NoError(t, err, "the pinned resolved member is stage recipe authority")
	require.Equal(t, stage.RecipeSHA256, stored.RecipeSHA256)
}

func TestProductionPageStageRetrySurvivesReopen(t *testing.T) {
	s, claim, proposed := productionPageStageFixture(t)
	first, err := s.StageProductionPage(t.Context(), claim, proposed)
	require.NoError(t, err)
	require.NotEmpty(t, first.SHA256)
	replayed, err := s.StageProductionPage(t.Context(), claim, proposed)
	require.NoError(t, err)
	require.Equal(t, first, replayed)
	reopened, err := Open(s.path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	loaded, err := reopened.LoadProductionPageStage(t.Context(), proposed.JobID, proposed.MemberID, proposed.Page)
	require.NoError(t, err)
	require.Equal(t, first, loaded)
	replayed, err = reopened.StageProductionPage(t.Context(), claim, proposed)
	require.NoError(t, err)
	require.Equal(t, first, replayed)
	var artifacts, stages int
	require.NoError(t, reopened.db.QueryRow(`SELECT COUNT(*) FROM production_job_artifacts WHERE job_id=?`, claim.JobID).Scan(&artifacts))
	require.NoError(t, reopened.db.QueryRow(`SELECT COUNT(*) FROM production_job_page_stages WHERE job_id=?`, claim.JobID).Scan(&stages))
	require.Equal(t, 1, artifacts)
	require.Equal(t, 1, stages)
}

func TestProductionPageStageRejectsStaleAndCanceledClaimAtomically(t *testing.T) {
	for _, mode := range []string{"stale", "canceled", "expired"} {
		t.Run(mode, func(t *testing.T) {
			s, claim, stage := productionPageStageFixture(t)
			switch mode {
			case "stale":
				claim.Epoch++
			case "canceled":
				require.NoError(t, s.CancelProductionJob(t.Context(), claim.JobID))
			case "expired":
				_, err := s.db.Exec(`UPDATE production_jobs SET lease_expires_at=? WHERE job_id=?`, "2020-01-01T00:00:00Z", claim.JobID)
				require.NoError(t, err)
			}
			_, err := s.StageProductionPage(t.Context(), claim, stage)
			require.ErrorIs(t, err, production.ErrJobStaleClaim)
			var artifacts, stages int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM production_job_artifacts WHERE job_id=?`, claim.JobID).Scan(&artifacts))
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM production_job_page_stages WHERE job_id=?`, claim.JobID).Scan(&stages))
			require.Zero(t, artifacts)
			require.Zero(t, stages)
		})
	}
}

func TestProductionPageStageRejectsChangedInputsAndTamper(t *testing.T) {
	for _, mode := range []string{"png", "artifact", "physical", "plan", "receipt", "canonical_bytes"} {
		t.Run(mode, func(t *testing.T) {
			s, claim, stage := productionPageStageFixture(t)
			_, err := s.StageProductionPage(t.Context(), claim, stage)
			require.NoError(t, err)
			switch mode {
			case "png":
				stage.Artifact.SHA256 = productionHash("changed PNG")
				_, err = s.StageProductionPage(t.Context(), claim, stage)
				require.ErrorIs(t, err, production.ErrJobConflict)
			case "artifact":
				_, err = s.db.Exec(`UPDATE production_job_artifacts SET artifact_json=? WHERE job_id=? AND artifact_id=?`, []byte(`{}`), claim.JobID, stage.Artifact.ID)
				require.NoError(t, err)
			case "physical":
				_, err = s.db.Exec(`DELETE FROM blob_locations WHERE blob_hash=?`, stage.Artifact.SHA256)
				require.NoError(t, err)
			case "plan":
				_, err = s.db.Exec(`DROP TRIGGER production_job_render_plans_immutable_update`)
				require.NoError(t, err)
				var planRaw []byte
				require.NoError(t, s.db.QueryRow(`SELECT canonical_json FROM production_job_render_plans WHERE job_id=?`, claim.JobID).Scan(&planRaw))
				plan, decodeErr := canonical.Decode[production.RenderPlan](planRaw)
				require.NoError(t, decodeErr)
				plan.Pages[0].ResolvedSHA256 = productionHash("changed resolved page")
				plan.SHA256 = ""
				plan, changedRaw, canonicalErr := production.CanonicalRenderPlan(plan)
				require.NoError(t, canonicalErr)
				_, err = s.db.Exec(`UPDATE production_job_render_plans SET canonical_json=?,plan_sha256=? WHERE job_id=?`, changedRaw, plan.SHA256, claim.JobID)
				require.NoError(t, err)
			case "receipt":
				_, err = s.db.Exec(`UPDATE production_job_page_stages SET stage_sha256=? WHERE job_id=?`, productionHash("tampered"), claim.JobID)
				require.Error(t, err, "immutable receipt trigger must stop ordinary mutation")
				_, err = s.db.Exec(`DROP TRIGGER production_job_page_stages_immutable_update`)
				require.NoError(t, err)
				_, err = s.db.Exec(`UPDATE production_job_page_stages SET stage_sha256=? WHERE job_id=?`, productionHash("tampered"), claim.JobID)
				require.NoError(t, err)
			case "canonical_bytes":
				_, err = s.db.Exec(`DROP TRIGGER production_job_page_stages_immutable_update`)
				require.NoError(t, err)
				var raw []byte
				require.NoError(t, s.db.QueryRow(`SELECT canonical_json FROM production_job_page_stages WHERE job_id=?`, claim.JobID).Scan(&raw))
				_, err = s.db.Exec(`UPDATE production_job_page_stages SET canonical_json=? WHERE job_id=?`, append(raw, ' '), claim.JobID)
				require.NoError(t, err)
			}
			if mode != "png" {
				reopened, openErr := Open(s.path)
				require.NoError(t, openErr)
				t.Cleanup(func() { require.NoError(t, reopened.Close()) })
				_, err = reopened.LoadProductionPageStage(t.Context(), stage.JobID, stage.MemberID, stage.Page)
				require.Error(t, err)
			}
		})
	}
}

func TestProductionPageStageSchemaIsRequiredAndPristine(t *testing.T) {
	s := newTestStore(t)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	var version int
	require.NoError(t, s.db.QueryRow(`SELECT schema_version FROM vault_metadata WHERE singleton=1`).Scan(&version))
	require.Equal(t, currentStorageSchemaVersion, version)
	_, err := s.db.Exec(`INSERT INTO production_job_page_stages(job_id,member_id,page,artifact_id,artifact_sha256,stage_sha256,canonical_json,created_at) VALUES('synthetic','synthetic',1,'synthetic','synthetic','synthetic','{}','2026-01-01T00:00:00Z')`)
	require.Error(t, err, "foreign key prevents orphan receipt")
	conn, err := s.db.Conn(t.Context())
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `INSERT INTO production_job_page_stages(job_id,member_id,page,artifact_id,artifact_sha256,stage_sha256,canonical_json,created_at) VALUES('synthetic','synthetic',1,'synthetic','synthetic','synthetic','{}','2026-01-01T00:00:00Z')`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=ON`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	err = s.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes()))
	require.ErrorContains(t, err, "not pristine")
	var rows int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM production_job_page_stages`).Scan(&rows))
	require.Equal(t, 1, rows)
}

func TestProductionPageStageReleasedUpgradeCreatesCurrentTable(t *testing.T) {
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "docbank.db")
			createV090Fixture(t, path, driver.driver)
			s, err := Open(path, driver.driver)
			require.NoError(t, err)
			defer func() { require.NoError(t, s.Close()) }()
			var version, stages int
			require.NoError(t, s.db.QueryRow(`SELECT schema_version FROM vault_metadata WHERE singleton=1`).Scan(&version))
			require.Equal(t, currentStorageSchemaVersion, version)
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM production_job_page_stages`).Scan(&stages))
			require.Zero(t, stages)
		})
	}
}
