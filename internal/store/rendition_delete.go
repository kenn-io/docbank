package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// deleteRenditionAuthorityForVersionsTx revokes version-specific serving
// authority before its owning content versions are permanently deleted.
// Immutable builds remain cataloged until ordinary derivative GC proves that
// no attachment or other root still needs them.
func deleteRenditionAuthorityForVersionsTx(
	ctx context.Context, tx *sql.Tx, versionIDs []string,
) (retErr error) {
	if len(versionIDs) == 0 {
		return nil
	}
	deleteHeads, err := tx.PrepareContext(ctx,
		`DELETE FROM rendition_heads WHERE content_version_id=?`)
	if err != nil {
		return fmt.Errorf("preparing rendition head deletion: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, deleteHeads.Close()) }()
	deleteAttachments, err := tx.PrepareContext(ctx,
		`DELETE FROM rendition_attachments WHERE content_version_id=?`)
	if err != nil {
		return fmt.Errorf("preparing rendition attachment deletion: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, deleteAttachments.Close()) }()
	deleteJobWaiters, err := tx.PrepareContext(ctx,
		`DELETE FROM rendition_job_waiters WHERE content_version_id=?`)
	if err != nil {
		return fmt.Errorf("preparing rendition job waiter deletion: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, deleteJobWaiters.Close()) }()
	for _, versionID := range versionIDs {
		if _, err := deleteHeads.ExecContext(ctx, versionID); err != nil {
			return fmt.Errorf("deleting rendition heads for content version %s: %w", versionID, err)
		}
		if _, err := deleteAttachments.ExecContext(ctx, versionID); err != nil {
			return fmt.Errorf("deleting rendition attachments for content version %s: %w", versionID, err)
		}
		if _, err := deleteJobWaiters.ExecContext(ctx, versionID); err != nil {
			return fmt.Errorf("deleting rendition job waiters for content version %s: %w", versionID, err)
		}
	}
	return reconcileRenditionJobsAfterWaiterDeletionTx(ctx, tx, nowRFC3339())
}

// reconcileRenditionJobsAfterWaiterDeletionTx reselects or requeues shared
// work whose selected authority disappeared, retaining staged roots while a
// consumer remains. Orphaned work is fenced and removed, except that ambiguous
// provider egress keeps a durable tombstone so a later enqueue cannot silently
// resubmit the same call.
func reconcileRenditionJobsAfterWaiterDeletionTx(
	ctx context.Context, tx *sql.Tx, at string,
) (retErr error) {
	if _, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET
		selected_waiter_id=NULL,authorization_grant_id=NULL,
		authorization_incarnation_id=NULL,authorization_revocation_fence=NULL,updated_at=?
		WHERE state IN ('completed','operator_required')
		  AND selected_waiter_id IS NOT NULL
		  AND NOT EXISTS (
			SELECT 1 FROM rendition_job_waiters w
			WHERE w.waiter_id=rendition_jobs.selected_waiter_id
		  )`, at); err != nil {
		return fmt.Errorf("clearing deleted terminal rendition authority: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT job_id,vault_uid,state,phase,provider_started,
		       COALESCE(failure_code,''),
		       provider_resume_handle IS NOT NULL AND execution_snapshot_json IS NOT NULL
		FROM rendition_jobs
		WHERE state IN ('queued','running','retry_wait','failed')
		  AND selected_waiter_id IS NOT NULL
		  AND NOT EXISTS (
			SELECT 1 FROM rendition_job_waiters w
			WHERE w.waiter_id=rendition_jobs.selected_waiter_id
		  )
		ORDER BY job_id`)
	if err != nil {
		return fmt.Errorf("listing rendition jobs with deleted authority: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	type affectedJob struct {
		id, vaultID      string
		state            RenditionJobState
		phase            RenditionJobPhase
		providerStarted  bool
		failureCode      RenditionFailureCode
		hasDurableResume bool
	}
	var affected []affectedJob
	for rows.Next() {
		var job affectedJob
		if err := rows.Scan(&job.id, &job.vaultID, &job.state, &job.phase,
			&job.providerStarted, &job.failureCode, &job.hasDurableResume); err != nil {
			return fmt.Errorf("reading rendition job with deleted authority: %w", err)
		}
		affected = append(affected, job)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("listing rendition jobs with deleted authority: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing rendition jobs with deleted authority: %w", err)
	}
	atTime, err := time.Parse(timestampLayout, at)
	if err != nil {
		return fmt.Errorf("parsing rendition waiter deletion time: %w", err)
	}
	for _, affectedJob := range affected {
		job, err := loadRenditionJobTx(ctx, tx, affectedJob.id)
		if err != nil {
			return fmt.Errorf("loading rendition job with deleted authority: %w", err)
		}
		safeFailure := affectedJob.failureCode == RenditionFailureConsent ||
			affectedJob.failureCode == RenditionFailureStaleAuthority
		safePhase := affectedJob.phase == RenditionPhaseQueued ||
			affectedJob.phase == RenditionPhaseBuildStaged ||
			affectedJob.phase == RenditionPhaseGenerationStaged ||
			affectedJob.phase == RenditionPhaseProvider && affectedJob.hasDurableResume
		if affectedJob.state == RenditionJobFailed && (!safeFailure || !safePhase) {
			if _, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET
				selected_waiter_id=NULL,authorization_grant_id=NULL,
				authorization_incarnation_id=NULL,authorization_revocation_fence=NULL,
				updated_at=? WHERE job_id=?`, at, job.ID); err != nil {
				return fmt.Errorf("clearing deleted failed rendition authority: %w", err)
			}
			continue
		}
		var hasWaiting bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM rendition_job_waiters WHERE job_id=? AND state='waiting'
		)`, job.ID).Scan(&hasWaiting); err != nil {
			return fmt.Errorf("checking remaining rendition waiters: %w", err)
		}
		if !hasWaiting {
			if _, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET
				state=CASE
					WHEN phase='provider' AND provider_started=1 THEN 'operator_required'
					ELSE 'failed'
				END,
				phase=CASE
					WHEN phase='provider' AND provider_started=1 THEN phase
					ELSE 'queued'
				END,
				claim_owner=NULL,lease_expires_at=NULL,selected_waiter_id=NULL,
				authorization_grant_id=NULL,authorization_incarnation_id=NULL,
				authorization_revocation_fence=NULL,
				provider_started=CASE
					WHEN phase='provider' AND provider_started=1 THEN provider_started
					ELSE 0
				END,
				provider_resume_handle=CASE
					WHEN phase='provider' AND provider_started=1 THEN provider_resume_handle
					ELSE NULL
				END,
				execution_snapshot_json=CASE
					WHEN phase='provider' AND provider_started=1 THEN execution_snapshot_json
					ELSE NULL
				END,
				lexical_generation_id=CASE
					WHEN phase='provider' AND provider_started=1 THEN lexical_generation_id
					ELSE NULL
				END,
				failure_code=CASE
					WHEN phase='provider' AND provider_started=1 THEN 'ambiguous'
					ELSE 'stale_authority'
				END,
				updated_at=? WHERE job_id=?`, at, job.ID); err != nil {
				return fmt.Errorf("fencing orphaned rendition job: %w", err)
			}
			continue
		}

		// An in-flight provider call must not be resumed concurrently, even
		// after it records a durable handle. Transfer publication authority to
		// another live waiter while preserving the current claim and call.
		if affectedJob.state == RenditionJobRunning &&
			affectedJob.phase == RenditionPhaseProvider && affectedJob.providerStarted {
			waiterID, authorization, selectErr := selectRenditionJobWaiterTx(
				ctx, tx, affectedJob.vaultID, job, atTime)
			if selectErr == nil {
				if _, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET
					selected_waiter_id=?,authorization_grant_id=?,
					authorization_incarnation_id=?,authorization_revocation_fence=?,updated_at=?
					WHERE job_id=?`, waiterID, authorization.GrantID,
					authorization.ProcessingIncarnationID, authorization.RevocationFence,
					at, job.ID); err != nil {
					return fmt.Errorf("reselecting in-flight rendition waiter: %w", err)
				}
				continue
			}
			if !errors.Is(selectErr, ErrProcessingConsentRequired) &&
				!errors.Is(selectErr, ErrRenditionJobStaleAuthority) {
				return selectErr
			}
			if _, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET
				state='operator_required',claim_owner=NULL,lease_expires_at=NULL,
				selected_waiter_id=NULL,authorization_grant_id=NULL,
				authorization_incarnation_id=NULL,authorization_revocation_fence=NULL,
				failure_code='ambiguous',updated_at=? WHERE job_id=?`, at, job.ID); err != nil {
				return fmt.Errorf("fencing unauthorized in-flight rendition job: %w", err)
			}
			continue
		}

		if _, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET
			state='queued',claim_owner=NULL,lease_expires_at=NULL,
			claim_epoch=claim_epoch+1,available_at=?,selected_waiter_id=NULL,
			authorization_grant_id=NULL,authorization_incarnation_id=NULL,
			authorization_revocation_fence=NULL,failure_code=NULL,updated_at=?
			WHERE job_id=?`, at, at, job.ID); err != nil {
			return fmt.Errorf("requeueing rendition job for remaining waiters: %w", err)
		}
		var generationID sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT lexical_generation_id FROM rendition_jobs WHERE job_id=?`, job.ID,
		).Scan(&generationID); err != nil {
			return fmt.Errorf("reading requeued rendition generation: %w", err)
		}
		if err := refreshRenditionJobRootsTx(
			ctx, tx, job.ID, affectedJob.phase, generationID, job.ClaimEpoch+1, at); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE current_rendition_roots SET
		active=0,released_at=?
		WHERE active=1 AND EXISTS (
			SELECT 1 FROM rendition_jobs j
			WHERE (current_rendition_roots.root_id='rendition_job_build_' || j.job_id
			       OR current_rendition_roots.root_id='rendition_job_generation_' || j.job_id)
			  AND NOT EXISTS (
				SELECT 1 FROM rendition_job_waiters w
				WHERE w.job_id=j.job_id AND w.state='waiting'
			  )
		)`, at); err != nil {
		return fmt.Errorf("releasing canceled rendition job roots: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM current_rendition_roots
		WHERE EXISTS (
			SELECT 1 FROM rendition_jobs j
			WHERE (current_rendition_roots.root_id='rendition_job_build_' || j.job_id
			       OR current_rendition_roots.root_id='rendition_job_generation_' || j.job_id)
			  AND j.state<>'operator_required'
			  AND NOT EXISTS (
				SELECT 1 FROM rendition_job_waiters w
				WHERE w.job_id=j.job_id AND w.state='waiting'
			  )
		)`); err != nil {
		return fmt.Errorf("deleting orphaned rendition job roots: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM rendition_jobs
		WHERE state<>'operator_required'
		  AND NOT EXISTS (
			SELECT 1 FROM rendition_job_waiters w WHERE w.job_id=rendition_jobs.job_id
		  )`); err != nil {
		return fmt.Errorf("deleting orphaned rendition jobs: %w", err)
	}
	return nil
}

// purgeRenditionJobWaitersTx applies explicit derivative selectors to pending
// consumers before GC computes reachability. It returns exact durable
// suppressions so the same authority cannot be silently enqueued again.
func purgeRenditionJobWaitersTx(
	ctx context.Context, tx *sql.Tx, versionSet, attachmentSet, buildSet map[string]struct{},
	all bool, purgedAt string,
) (_ []derivativePurgeSuppression, retErr error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT j.job_id,j.source_sha256,w.waiter_id,w.content_version_id,
		       w.profile_fingerprint,w.attachment_id
		FROM rendition_jobs j
		LEFT JOIN rendition_job_waiters w ON w.job_id=j.job_id
		ORDER BY j.job_id,w.waiter_id`)
	if err != nil {
		return nil, fmt.Errorf("listing rendition jobs affected by derivative purge: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	var waiterIDs []string
	scopes := make([]derivativePurgeSuppression, 0)
	seenScopes := make(map[string]struct{})
	matched := false
	for rows.Next() {
		var jobID, sourceSHA256 string
		var waiterID, versionID, profileFingerprint, attachmentID sql.NullString
		if err := rows.Scan(&jobID, &sourceSHA256, &waiterID, &versionID,
			&profileFingerprint, &attachmentID); err != nil {
			return nil, fmt.Errorf("reading rendition job affected by derivative purge: %w", err)
		}
		_, selectedBuild := buildSet[jobID]
		selectedJob := all || selectedBuild
		selectedWaiter := waiterID.Valid && selectedJob
		if waiterID.Valid && !selectedWaiter {
			_, selectedVersion := versionSet[versionID.String]
			_, selectedAttachment := attachmentSet[attachmentID.String]
			selectedWaiter = selectedVersion || selectedAttachment
		}
		if !selectedJob && !selectedWaiter {
			continue
		}
		matched = true
		if selectedJob {
			scope := derivativePurgeSuppression{
				sourceSHA256: sourceSHA256, profileFingerprint: derivativeBuildSuppressionProfile,
				buildID: jobID, purgedAt: purgedAt, active: true,
			}
			key := scope.sourceSHA256 + "\x00" + scope.profileFingerprint + "\x00" + scope.buildID
			if _, exists := seenScopes[key]; !exists {
				seenScopes[key] = struct{}{}
				scopes = append(scopes, scope)
			}
		} else {
			scope := derivativePurgeSuppression{
				sourceSHA256: sourceSHA256,
				profileFingerprint: derivativeAttachmentSuppressionScope(
					versionID.String, profileFingerprint.String),
				buildID: jobID, purgedAt: purgedAt, active: true,
			}
			key := scope.sourceSHA256 + "\x00" + scope.profileFingerprint + "\x00" + scope.buildID
			if _, exists := seenScopes[key]; !exists {
				seenScopes[key] = struct{}{}
				scopes = append(scopes, scope)
			}
		}
		if selectedWaiter {
			waiterIDs = append(waiterIDs, waiterID.String)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing rendition jobs affected by derivative purge: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("closing rendition jobs affected by derivative purge: %w", err)
	}
	for _, waiterID := range waiterIDs {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM rendition_job_waiters WHERE waiter_id=?`, waiterID); err != nil {
			return nil, fmt.Errorf("deleting purged rendition job waiter %s: %w", waiterID, err)
		}
	}
	if matched {
		if err := reconcileRenditionJobsAfterWaiterDeletionTx(ctx, tx, purgedAt); err != nil {
			return nil, err
		}
	}
	return scopes, nil
}
