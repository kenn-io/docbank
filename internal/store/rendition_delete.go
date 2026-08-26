package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	if err := deleteEmbeddingAuthorityForVersionsTx(ctx, tx, versionIDs); err != nil {
		return err
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

// deleteEmbeddingAuthorityForVersionsTx removes dependent derivative
// authority before the owning version and its rendition attachment. Immutable
// vector/generation authorities shared by another set remain intact.
func deleteEmbeddingAuthorityForVersionsTx(ctx context.Context, tx *sql.Tx, versionIDs []string) error {
	for _, versionID := range versionIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM current_rendition_roots WHERE
			target_kind='embedding_set' AND target_id IN (
				SELECT embedding_set_id FROM embedding_sets WHERE content_version_id=?)`,
			versionID); err != nil {
			return fmt.Errorf("releasing embedding roots for content version %s: %w", versionID, err)
		}
		setIDs, err := stringColumnTx(ctx, tx, "embedding sets for content deletion",
			`SELECT embedding_set_id FROM embedding_sets WHERE content_version_id=? ORDER BY embedding_set_id`, versionID)
		if err != nil {
			return err
		}
		for _, setID := range setIDs {
			if _, err := tx.ExecContext(ctx, `DELETE FROM embedding_corpus_generations WHERE corpus_generation_id IN (
				SELECT corpus_generation_id FROM embedding_corpus_members WHERE embedding_set_id=?)`, setID); err != nil {
				return fmt.Errorf("deleting embedding corpora for content version %s: %w", versionID, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM embedding_heads WHERE content_version_id=?`, versionID); err != nil {
			return fmt.Errorf("deleting embedding heads for content version %s: %w", versionID, err)
		}
		var generationIDs, vectorSetIDs, vectorSpaceIDs, payloadIDs []string
		for _, query := range []struct {
			destination *[]string
			label, sql  string
		}{
			{&generationIDs, "embedding generations", `SELECT DISTINCT input_generation_id FROM embedding_sets WHERE content_version_id=? ORDER BY input_generation_id`},
			{&vectorSetIDs, "embedding vector sets", `SELECT DISTINCT vector_set_id FROM embedding_sets WHERE content_version_id=? ORDER BY vector_set_id`},
			{&vectorSpaceIDs, "embedding vector spaces", `SELECT DISTINCT vector_space_id FROM embedding_sets WHERE content_version_id=? ORDER BY vector_space_id`},
			{&payloadIDs, "embedding payloads", `SELECT DISTINCT v.payload_blob_hash FROM embedding_sets s JOIN embedding_vector_sets v ON v.vector_set_id=s.vector_set_id WHERE s.content_version_id=? ORDER BY v.payload_blob_hash`},
		} {
			values, err := stringColumnTx(ctx, tx, query.label, query.sql, versionID)
			if err != nil {
				return err
			}
			*query.destination = values
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM embedding_sets WHERE content_version_id=?`, versionID); err != nil {
			return fmt.Errorf("deleting embedding sets for content version %s: %w", versionID, err)
		}
		for _, id := range generationIDs {
			result, err := tx.ExecContext(ctx, `DELETE FROM embedding_input_generations WHERE generation_id=? AND NOT EXISTS(
				SELECT 1 FROM embedding_sets WHERE input_generation_id=?)`, id, id)
			if err != nil {
				return err
			}
			if deleted, _ := result.RowsAffected(); deleted > 0 {
				if _, err := tx.ExecContext(ctx, `DELETE FROM current_rendition_roots WHERE target_kind='embedding_input_generation' AND target_id=?`, id); err != nil {
					return err
				}
			}
		}
		for _, id := range vectorSetIDs {
			result, err := tx.ExecContext(ctx, `DELETE FROM embedding_vector_sets WHERE vector_set_id=? AND NOT EXISTS(
				SELECT 1 FROM embedding_sets WHERE vector_set_id=?)`, id, id)
			if err != nil {
				return err
			}
			if deleted, _ := result.RowsAffected(); deleted > 0 {
				if _, err := tx.ExecContext(ctx, `DELETE FROM current_rendition_roots WHERE target_kind='embedding_vector_set' AND target_id=?`, id); err != nil {
					return err
				}
			}
		}
		for _, id := range payloadIDs {
			if _, err := tx.ExecContext(ctx, `DELETE FROM current_rendition_roots
				WHERE target_kind='embedding_payload' AND target_id=? AND NOT EXISTS(
					SELECT 1 FROM embedding_vector_sets WHERE payload_blob_hash=?)`, id, id); err != nil {
				return err
			}
		}
		for _, id := range vectorSpaceIDs {
			if _, err := tx.ExecContext(ctx, `DELETE FROM embedding_vector_spaces WHERE vector_space_id=?
				AND NOT EXISTS(SELECT 1 FROM embedding_sets WHERE vector_space_id=?)
				AND NOT EXISTS(SELECT 1 FROM embedding_corpus_generations WHERE vector_space_id=?)`, id, id, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconcileRenditionJobsAfterWaiterDeletionTx fences work whose selected
// authority disappeared, releases roots that can no longer support a live
// claim, and removes jobs with no consumers. Ambiguous provider egress is the
// exception: its tombstone must remain durable so a later enqueue cannot
// silently resubmit the same call.
func reconcileRenditionJobsAfterWaiterDeletionTx(
	ctx context.Context, tx *sql.Tx, at string,
) error {
	if _, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET
		state=CASE
			WHEN phase='provider' AND provider_started=1 THEN 'operator_required'
			ELSE 'failed'
		END,
		claim_owner=NULL,lease_expires_at=NULL,selected_waiter_id=NULL,
		authorization_grant_id=NULL,authorization_incarnation_id=NULL,
		authorization_revocation_fence=NULL,
		failure_code=CASE
			WHEN phase='provider' AND provider_started=1 THEN 'ambiguous'
			ELSE 'stale_authority'
		END,
		updated_at=?
		WHERE state IN ('queued','running','retry_wait')
		  AND selected_waiter_id IS NOT NULL
		  AND NOT EXISTS (
			SELECT 1 FROM rendition_job_waiters w
			WHERE w.waiter_id=rendition_jobs.selected_waiter_id
		  )`, at); err != nil {
		return fmt.Errorf("fencing rendition jobs with deleted authority: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE current_rendition_roots SET
		active=0,released_at=?
		WHERE active=1 AND EXISTS (
			SELECT 1 FROM rendition_jobs j
			WHERE (current_rendition_roots.root_id='rendition_job_build_' || j.job_id
			       OR current_rendition_roots.root_id='rendition_job_generation_' || j.job_id)
			  AND (j.state<>'running' OR NOT EXISTS (
				SELECT 1 FROM rendition_job_waiters w WHERE w.job_id=j.job_id
			  ))
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
				SELECT 1 FROM rendition_job_waiters w WHERE w.job_id=j.job_id
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
