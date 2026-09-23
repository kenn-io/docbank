package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"go.kenn.io/docbank/internal/canonical"
	productionservice "go.kenn.io/docbank/internal/production"
)

// CheckpointProductionRenderPlan retains one bounded, ordered plan before a
// render-stage claim. Exact retries return the original row; divergent plans
// cannot replace it, including after a lost acknowledgement or restart.
func (s *Store) CheckpointProductionRenderPlan(ctx context.Context, job productionservice.Job, proposal productionservice.RenderPlan) (productionservice.RenderPlan, error) {
	plan, raw, err := productionservice.CanonicalRenderPlan(proposal)
	if err != nil || plan.JobID != job.ID || plan.RevisionSHA256 != job.RevisionSHA256 {
		return productionservice.RenderPlan{}, productionservice.ErrJobConflict
	}
	finalized, err := s.LoadFinalizedProduction(ctx, job.SetID, job.Revision)
	if err != nil || finalized.Authority.Receipt == nil || finalized.Draft.ETag != job.ETag ||
		finalized.Authority.Prepared.SHA256 != job.RevisionSHA256 || finalized.Authority.Receipt.SHA256 != job.PreparedInputSHA256 {
		return productionservice.RenderPlan{}, productionservice.ErrJobConflict
	}
	allocation, err := s.ProductionNumberingForJob(ctx, job.ID)
	if err != nil || allocation.AllocationID != plan.Reservation.ID ||
		allocation.State == batesAllocationStateAbandoned ||
		len(allocation.Labels) != len(plan.Reservation.Numbers) {
		return productionservice.RenderPlan{}, productionservice.ErrJobConflict
	}
	for index, label := range allocation.Labels {
		number := plan.Reservation.Numbers[index]
		if number.MemberID != label.OccurrenceID || number.MemberOrdinal != int64(label.Ordinal) ||
			number.Page != label.SourcePage || number.Text != label.Label {
			return productionservice.RenderPlan{}, productionservice.ErrJobConflict
		}
	}
	recipeTx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return productionservice.RenderPlan{}, err
	}
	recipe, err := loadProductionRecipeTx(ctx, recipeTx, finalized.Draft)
	if err != nil {
		_ = recipeTx.Rollback()
		return productionservice.RenderPlan{}, productionservice.ErrJobConflict
	}
	if err := recipeTx.Commit(); err != nil {
		return productionservice.RenderPlan{}, err
	}
	pageIndex := 0
	for _, prepared := range finalized.Authority.Prepared.Members {
		endorsed, err := productionservice.PlanEndorsementPages(prepared.Member.ID, prepared.Resolved.Pages,
			prepared.Resolved, plan.Reservation.Numbers, recipe)
		if err != nil || len(endorsed) != len(prepared.Resolved.Pages) {
			return productionservice.RenderPlan{}, productionservice.ErrJobConflict
		}
		for index, page := range prepared.Resolved.Pages {
			if pageIndex >= len(plan.Pages) {
				return productionservice.RenderPlan{}, productionservice.ErrJobConflict
			}
			planned := plan.Pages[pageIndex]
			if planned.MemberID != prepared.Member.ID || planned.MemberOrdinal != prepared.Member.Ordinal ||
				planned.Page != page.Number || planned.ResolvedSHA256 != prepared.ResolvedSHA256 ||
				planned.Layout != endorsed[index].Layout || !slices.Equal(planned.Endorsements, endorsed[index].Endorsements) ||
				planned.Layout.Source != page {
				return productionservice.RenderPlan{}, productionservice.ErrJobConflict
			}
			pageIndex++
		}
	}
	if pageIndex != len(plan.Pages) {
		return productionservice.RenderPlan{}, productionservice.ErrJobConflict
	}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var setID, revisionSHA, preparedSHA, state string
		var boundAllocation sql.NullString
		var revision, etag int64
		if err := tx.QueryRowContext(ctx, `SELECT set_id,revision,etag,revision_sha256,prepared_input_sha256,state,allocation_id FROM production_jobs WHERE job_id=?`, job.ID).
			Scan(&setID, &revision, &etag, &revisionSHA, &preparedSHA, &state, &boundAllocation); err != nil ||
			setID != job.SetID || revision != job.Revision || etag != job.ETag ||
			revisionSHA != job.RevisionSHA256 || preparedSHA != job.PreparedInputSHA256 ||
			boundAllocation.Valid && boundAllocation.String != plan.Reservation.ID {
			return productionservice.ErrJobConflict
		}
		bindAllocation := func() error {
			if boundAllocation.Valid {
				return nil
			}
			updated, err := tx.ExecContext(ctx, `UPDATE production_jobs SET allocation_id=? WHERE job_id=? AND allocation_id IS NULL`,
				plan.Reservation.ID, job.ID)
			if err != nil {
				return err
			}
			if count, err := updated.RowsAffected(); err != nil || count != 1 {
				return productionservice.ErrJobConflict
			}
			return nil
		}
		var existingSHA string
		var existingRaw []byte
		err := tx.QueryRowContext(ctx, `SELECT plan_sha256,canonical_json FROM production_job_render_plans WHERE job_id=?`, job.ID).Scan(&existingSHA, &existingRaw)
		if err == nil {
			if existingSHA != plan.SHA256 || !bytes.Equal(existingRaw, raw) {
				return productionservice.ErrJobConflict
			}
			return bindAllocation()
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if state != productionservice.ProductionJobQueued {
			return productionservice.ErrJobConflict
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO production_job_render_plans(job_id,allocation_id,reservation_sha256,plan_sha256,canonical_json,created_at) VALUES(?,?,?,?,?,?)`,
			job.ID, plan.Reservation.ID, plan.Reservation.SHA256, plan.SHA256, raw, nowRFC3339())
		if err != nil {
			return err
		}
		return bindAllocation()
	})
	if err != nil {
		return productionservice.RenderPlan{}, err
	}
	return plan, nil
}

// LoadProductionRenderPlan verifies the retained canonical bytes and digest.
func (s *Store) LoadProductionRenderPlan(ctx context.Context, jobID string) (productionservice.RenderPlan, error) {
	if validateUUIDv4(jobID) != nil {
		return productionservice.RenderPlan{}, productionservice.ErrJobConflict
	}
	var size int64
	err := s.db.QueryRowContext(ctx, `SELECT length(canonical_json) FROM production_job_render_plans WHERE job_id=?`, jobID).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return productionservice.RenderPlan{}, ErrNotFound
	}
	if err != nil {
		return productionservice.RenderPlan{}, err
	}
	if size < 1 || size > productionservice.MaxRenderPlanBytes {
		return productionservice.RenderPlan{}, ErrInvalidProduction
	}
	var raw []byte
	var allocationID, reservationSHA, planSHA string
	err = s.db.QueryRowContext(ctx, `SELECT allocation_id,reservation_sha256,plan_sha256,canonical_json FROM production_job_render_plans WHERE job_id=?`, jobID).
		Scan(&allocationID, &reservationSHA, &planSHA, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return productionservice.RenderPlan{}, ErrNotFound
	}
	if err != nil {
		return productionservice.RenderPlan{}, err
	}
	if len(raw) > productionservice.MaxRenderPlanBytes {
		return productionservice.RenderPlan{}, ErrInvalidProduction
	}
	plan, err := canonical.Decode[productionservice.RenderPlan](raw)
	if err != nil {
		return productionservice.RenderPlan{}, ErrInvalidProduction
	}
	canonicalPlan, canonicalRaw, err := productionservice.CanonicalRenderPlan(plan)
	if err != nil || !bytes.Equal(raw, canonicalRaw) || planSHA != canonicalPlan.SHA256 ||
		allocationID != canonicalPlan.Reservation.ID || reservationSHA != canonicalPlan.Reservation.SHA256 ||
		jobID != canonicalPlan.JobID {
		return productionservice.RenderPlan{}, ErrInvalidProduction
	}
	return canonicalPlan, nil
}

// ClaimProductionRenderStage refuses a render claim until a matching plan is
// durably available for restart. Existing generic job claims remain for the
// pre-render lifecycle until Task J replaces that path.
func (s *Store) ClaimProductionRenderStage(ctx context.Context, jobID, worker string, leaseDuration time.Duration) (productionservice.JobClaim, error) {
	plan, err := s.LoadProductionRenderPlan(ctx, jobID)
	if errors.Is(err, ErrNotFound) {
		return productionservice.JobClaim{}, productionservice.ErrJobIncomplete
	}
	if err != nil {
		return productionservice.JobClaim{}, err
	}
	var setID string
	var revision int64
	if err := s.db.QueryRowContext(ctx, `SELECT set_id,revision FROM production_jobs WHERE job_id=?`, jobID).Scan(&setID, &revision); err != nil {
		return productionservice.JobClaim{}, productionservice.ErrJobConflict
	}
	finalized, err := s.LoadFinalizedProduction(ctx, setID, revision)
	if err != nil || finalized.Authority.Prepared.SHA256 != plan.RevisionSHA256 {
		return productionservice.JobClaim{}, productionservice.ErrJobConflict
	}
	allocation, err := s.ProductionNumberingForJob(ctx, jobID)
	if err != nil || allocation.AllocationID != plan.Reservation.ID || allocation.State == batesAllocationStateAbandoned {
		return productionservice.JobClaim{}, productionservice.ErrJobConflict
	}
	return s.ClaimProductionJob(ctx, jobID, worker, leaseDuration)
}
