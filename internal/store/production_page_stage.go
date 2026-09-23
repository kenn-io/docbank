package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	productionservice "go.kenn.io/docbank/internal/production"
)

func productionPagePlanDigests(page productionservice.RenderPagePlan) (string, string, error) {
	layout, err := canonical.Marshal(page.Layout)
	if err != nil {
		return "", "", err
	}
	endorsements := page.Endorsements
	if endorsements == nil {
		endorsements = []redaction.Endorsement{}
	}
	endorsementRaw, err := canonical.Marshal(endorsements)
	if err != nil {
		return "", "", err
	}
	layoutSum, endorsementSum := sha256.Sum256(layout), sha256.Sum256(endorsementRaw)
	return hex.EncodeToString(layoutSum[:]), hex.EncodeToString(endorsementSum[:]), nil
}

func loadProductionPagePlanTx(ctx context.Context, tx *sql.Tx, jobID string) (productionservice.RenderPlan, string, string, documentproduction.PreparedInputAuthority, error) {
	var size int64
	err := tx.QueryRowContext(ctx, `SELECT length(canonical_json) FROM production_job_render_plans WHERE job_id=?`, jobID).Scan(&size)
	if err != nil || size < 1 || size > productionservice.MaxRenderPlanBytes {
		return productionservice.RenderPlan{}, "", "", documentproduction.PreparedInputAuthority{}, productionservice.ErrJobConflict
	}
	var raw, authorityRaw []byte
	var planSHA, allocationID, reservationSHA, revisionSHA, preparedSHA, finalizedRevisionSHA, finalizedPreparedSHA string
	err = tx.QueryRowContext(ctx, `SELECT p.canonical_json,p.plan_sha256,p.allocation_id,p.reservation_sha256,
		j.revision_sha256,j.prepared_input_sha256,f.prepared_sha256,f.prepared_input_sha256,f.prepared_input_json
		FROM production_job_render_plans p JOIN production_jobs j ON j.job_id=p.job_id
		JOIN production_finalized_revisions f ON f.set_id=j.set_id AND f.revision=j.revision
		WHERE p.job_id=?`, jobID).Scan(&raw, &planSHA, &allocationID, &reservationSHA, &revisionSHA,
		&preparedSHA, &finalizedRevisionSHA, &finalizedPreparedSHA, &authorityRaw)
	if err != nil {
		return productionservice.RenderPlan{}, "", "", documentproduction.PreparedInputAuthority{}, productionservice.ErrJobConflict
	}
	plan, err := canonical.Decode[productionservice.RenderPlan](raw)
	if err != nil {
		return productionservice.RenderPlan{}, "", "", documentproduction.PreparedInputAuthority{}, productionservice.ErrJobConflict
	}
	plan, canonicalRaw, err := productionservice.CanonicalRenderPlan(plan)
	if err != nil || !bytes.Equal(raw, canonicalRaw) || plan.SHA256 != planSHA || plan.JobID != jobID ||
		plan.RevisionSHA256 != revisionSHA || plan.Reservation.ID != allocationID || plan.Reservation.SHA256 != reservationSHA {
		return productionservice.RenderPlan{}, "", "", documentproduction.PreparedInputAuthority{}, productionservice.ErrJobConflict
	}
	authority, err := canonical.Decode[documentproduction.PreparedInputAuthority](authorityRaw)
	if err != nil || documentproduction.ValidatePreparedInputAuthority(authority) != nil ||
		authority.Prepared.SHA256 != revisionSHA || authority.Prepared.SHA256 != finalizedRevisionSHA ||
		authority.Receipt == nil || authority.Receipt.SHA256 != preparedSHA || authority.Receipt.SHA256 != finalizedPreparedSHA {
		return productionservice.RenderPlan{}, "", "", documentproduction.PreparedInputAuthority{}, productionservice.ErrJobConflict
	}
	return plan, revisionSHA, preparedSHA, authority, nil
}

type productionPageKey struct {
	memberID string
	page     int
}

type productionPageProof struct {
	ordinal                                            int64
	resolvedSHA, recipeSHA, layoutSHA, endorsementsSHA string
}

// A Store retains only its most recently used immutable job plan. Rendering
// and reopening many pages then require one full plan decode per job, not one
// per page. SQLite's immutable plan/finalization triggers guard this cache;
// a new Store verifies the canonical rows again after restart.
type productionPageProofCache struct {
	jobID, revisionSHA, preparedSHA, reservationSHA, planSHA string
	schemaVersion                                            int64
	pages                                                    map[productionPageKey]productionPageProof
}

func buildProductionPageProofCache(plan productionservice.RenderPlan, revisionSHA, preparedSHA string,
	authority documentproduction.PreparedInputAuthority) (*productionPageProofCache, error) {
	prepared := make(map[string]documentproduction.PreparedMember, len(authority.Prepared.Members))
	for _, member := range authority.Prepared.Members {
		prepared[member.Member.ID] = member
	}
	cache := &productionPageProofCache{jobID: plan.JobID, revisionSHA: revisionSHA, preparedSHA: preparedSHA,
		reservationSHA: plan.Reservation.SHA256, planSHA: plan.SHA256,
		pages: make(map[productionPageKey]productionPageProof, len(plan.Pages))}
	for _, page := range plan.Pages {
		member, ok := prepared[page.MemberID]
		if !ok || member.Member.Ordinal != page.MemberOrdinal || member.ResolvedSHA256 != page.ResolvedSHA256 {
			return nil, productionservice.ErrJobConflict
		}
		key := productionPageKey{page.MemberID, page.Page}
		if _, duplicate := cache.pages[key]; duplicate {
			return nil, productionservice.ErrJobConflict
		}
		layoutSHA, endorsementsSHA, err := productionPagePlanDigests(page)
		if err != nil {
			return nil, productionservice.ErrJobConflict
		}
		cache.pages[key] = productionPageProof{resolvedSHA: page.ResolvedSHA256,
			recipeSHA: member.Resolved.RecipeSHA256, layoutSHA: layoutSHA, endorsementsSHA: endorsementsSHA,
			ordinal: page.MemberOrdinal}
	}
	return cache, nil
}

func (s *Store) validateProductionPageStagePlanTx(ctx context.Context, tx *sql.Tx, stage productionservice.ProductionPageStage) error {
	var planSHA, revisionSHA, preparedSHA, finalizedRevisionSHA, finalizedPreparedSHA string
	err := tx.QueryRowContext(ctx, `SELECT p.plan_sha256,j.revision_sha256,j.prepared_input_sha256,
		f.prepared_sha256,f.prepared_input_sha256 FROM production_job_render_plans p
		JOIN production_jobs j ON j.job_id=p.job_id
		JOIN production_finalized_revisions f ON f.set_id=j.set_id AND f.revision=j.revision
		WHERE p.job_id=?`, stage.JobID).Scan(&planSHA, &revisionSHA, &preparedSHA, &finalizedRevisionSHA, &finalizedPreparedSHA)
	if err != nil || revisionSHA != finalizedRevisionSHA || preparedSHA != finalizedPreparedSHA {
		return productionservice.ErrJobConflict
	}
	var schemaVersion int64
	if err := tx.QueryRowContext(ctx, `PRAGMA schema_version`).Scan(&schemaVersion); err != nil {
		return err
	}
	s.pageStageProofMu.RLock()
	cache := s.pageStageProof
	s.pageStageProofMu.RUnlock()
	if cache == nil || cache.jobID != stage.JobID || cache.planSHA != planSHA ||
		cache.revisionSHA != revisionSHA || cache.preparedSHA != preparedSHA || cache.schemaVersion != schemaVersion {
		plan, revisionSHA, preparedSHA, authority, err := loadProductionPagePlanTx(ctx, tx, stage.JobID)
		if err != nil {
			return productionservice.ErrJobConflict
		}
		cache, err = buildProductionPageProofCache(plan, revisionSHA, preparedSHA, authority)
		if err != nil {
			return err
		}
		var triggers int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name IN (
			'production_job_render_plans_immutable_update','production_job_render_plans_immutable_delete',
			'production_finalized_revisions_immutable_update','production_finalized_revisions_immutable_delete')`).Scan(&triggers); err != nil {
			return err
		}
		cache.schemaVersion = schemaVersion
		s.pageStageProofMu.Lock()
		if triggers == 4 {
			s.pageStageProof = cache
		} else {
			// Without the triggers, digest columns do not prove that the
			// canonical bytes stayed unchanged. Re-read on each operation.
			s.pageStageProof = nil
		}
		s.pageStageProofMu.Unlock()
	}
	proof, found := cache.pages[productionPageKey{stage.MemberID, stage.Page}]
	if !found || stage.RevisionSHA256 != cache.revisionSHA || stage.PreparedInputSHA256 != cache.preparedSHA ||
		stage.ReservationSHA256 != cache.reservationSHA || stage.RenderPlanSHA256 != cache.planSHA ||
		stage.MemberOrdinal != proof.ordinal || stage.ResolvedSHA256 != proof.resolvedSHA ||
		stage.RecipeSHA256 != proof.recipeSHA || stage.LayoutSHA256 != proof.layoutSHA ||
		stage.EndorsementsSHA256 != proof.endorsementsSHA {
		return productionservice.ErrJobConflict
	}
	return nil
}

func validateProductionPageStagePhysicalTx(tx *sql.Tx, stage productionservice.ProductionPageStage) error {
	size, err := requirePhysicalAuthorityTx(tx, stage.Artifact.SHA256)
	if err != nil || size != stage.Artifact.Size {
		return productionservice.ErrJobConflict
	}
	return nil
}

func (s *Store) loadProductionPageStageTx(ctx context.Context, tx *sql.Tx, jobID, memberID string, page int) (productionservice.ProductionPageStage, error) {
	var size int64
	err := tx.QueryRowContext(ctx, `SELECT length(canonical_json) FROM production_job_page_stages WHERE job_id=? AND member_id=? AND page=?`,
		jobID, memberID, page).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return productionservice.ProductionPageStage{}, ErrNotFound
	}
	if err != nil || size < 1 || size > productionservice.MaxProductionPageStageBytes {
		return productionservice.ProductionPageStage{}, productionservice.ErrJobConflict
	}
	var artifactID, artifactSHA, digest string
	var raw, artifactRaw []byte
	err = tx.QueryRowContext(ctx, `SELECT artifact_id,artifact_sha256,stage_sha256,canonical_json FROM production_job_page_stages
		WHERE job_id=? AND member_id=? AND page=?`, jobID, memberID, page).Scan(&artifactID, &artifactSHA, &digest, &raw)
	if err != nil {
		return productionservice.ProductionPageStage{}, productionservice.ErrJobConflict
	}
	stage, err := canonical.Decode[productionservice.ProductionPageStage](raw)
	if err != nil {
		return productionservice.ProductionPageStage{}, productionservice.ErrJobConflict
	}
	stage, canonicalRaw, err := productionservice.CanonicalProductionPageStage(stage)
	if err != nil || !bytes.Equal(raw, canonicalRaw) || stage.SHA256 != digest ||
		stage.JobID != jobID || stage.MemberID != memberID || stage.Page != page ||
		stage.Artifact.ID != artifactID || stage.Artifact.SHA256 != artifactSHA {
		return productionservice.ProductionPageStage{}, productionservice.ErrJobConflict
	}
	err = tx.QueryRowContext(ctx, `SELECT artifact_json FROM production_job_artifacts WHERE job_id=? AND artifact_id=?`, jobID, artifactID).Scan(&artifactRaw)
	if err != nil {
		return productionservice.ProductionPageStage{}, productionservice.ErrJobConflict
	}
	expectedArtifactRaw, err := canonical.Marshal(stage.Artifact)
	if err != nil || !bytes.Equal(artifactRaw, expectedArtifactRaw) {
		return productionservice.ProductionPageStage{}, productionservice.ErrJobConflict
	}
	if err := validateProductionPageStagePhysicalTx(tx, stage); err != nil {
		return productionservice.ProductionPageStage{}, err
	}
	if err := s.validateProductionPageStagePlanTx(ctx, tx, stage); err != nil {
		return productionservice.ProductionPageStage{}, productionservice.ErrJobConflict
	}
	return stage, nil
}

// StageProductionPage records an artifact and its page receipt atomically.
// A retry is accepted only while the exact fenced claim is live and all
// persisted bytes still match the sealed plan and original stage.
func (s *Store) StageProductionPage(ctx context.Context, claim productionservice.JobClaim, proposal productionservice.ProductionPageStage) (productionservice.ProductionPageStage, error) {
	stage, raw, err := productionservice.CanonicalProductionPageStage(proposal)
	if err != nil || claim.JobID != stage.JobID || claim.Token == "" || claim.Worker == "" || claim.Epoch < 1 {
		return productionservice.ProductionPageStage{}, productionservice.ErrJobConflict
	}
	var result productionservice.ProductionPageStage
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var state, token, owner, lease string
		var epoch int64
		var canceled int
		if err := tx.QueryRowContext(ctx, `SELECT state,claim_epoch,claim_token,claim_owner,lease_expires_at,cancel_requested
			FROM production_jobs WHERE job_id=?`, stage.JobID).Scan(&state, &epoch, &token, &owner, &lease, &canceled); err != nil {
			return productionservice.ErrJobStaleClaim
		}
		expires, parseErr := time.Parse(time.RFC3339Nano, lease)
		if parseErr != nil || state != productionservice.ProductionJobRunning || canceled != 0 ||
			epoch != claim.Epoch || token != claim.Token || owner != claim.Worker || !expires.After(time.Now().UTC()) {
			return productionservice.ErrJobStaleClaim
		}
		if err := s.validateProductionPageStagePlanTx(ctx, tx, stage); err != nil {
			return productionservice.ErrJobConflict
		}
		if err := validateProductionPageStagePhysicalTx(tx, stage); err != nil {
			return err
		}
		prior, err := s.loadProductionPageStageTx(ctx, tx, stage.JobID, stage.MemberID, stage.Page)
		if err == nil {
			if prior != stage {
				return productionservice.ErrJobConflict
			}
			result = prior
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		artifactRaw, err := canonical.Marshal(stage.Artifact)
		if err != nil {
			return productionservice.ErrJobConflict
		}
		var existing []byte
		err = tx.QueryRowContext(ctx, `SELECT artifact_json FROM production_job_artifacts WHERE job_id=? AND artifact_id=?`,
			stage.JobID, stage.Artifact.ID).Scan(&existing)
		if err == nil && !bytes.Equal(existing, artifactRaw) {
			return productionservice.ErrJobConflict
		}
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, `INSERT INTO production_job_artifacts(job_id,artifact_id,artifact_json,created_at) VALUES(?,?,?,?)`,
				stage.JobID, stage.Artifact.ID, artifactRaw, nowRFC3339())
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO production_job_page_stages
			(job_id,member_id,page,artifact_id,artifact_sha256,stage_sha256,canonical_json,created_at) VALUES(?,?,?,?,?,?,?,?)`,
			stage.JobID, stage.MemberID, stage.Page, stage.Artifact.ID, stage.Artifact.SHA256, stage.SHA256, raw, nowRFC3339())
		if err != nil {
			return err
		}
		result = stage
		return nil
	})
	return result, err
}

// LoadProductionPageStage checks the receipt, referenced plan and artifact
// after restart before the renderer can reuse a staged page handle.
func (s *Store) LoadProductionPageStage(ctx context.Context, jobID, memberID string, page int) (productionservice.ProductionPageStage, error) {
	if validateUUIDv4(jobID) != nil || memberID == "" || page < 1 {
		return productionservice.ProductionPageStage{}, productionservice.ErrJobConflict
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return productionservice.ProductionPageStage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	stage, err := s.loadProductionPageStageTx(ctx, tx, jobID, memberID, page)
	if err != nil {
		return productionservice.ProductionPageStage{}, err
	}
	if err := tx.Commit(); err != nil {
		return productionservice.ProductionPageStage{}, err
	}
	return stage, nil
}
