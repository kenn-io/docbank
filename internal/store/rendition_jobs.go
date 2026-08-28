package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

// RenditionJobState is the bounded durable execution state exposed to
// aggregate status. Provider bodies and source data are never job status.
type RenditionJobState string

const (
	RenditionJobQueued           RenditionJobState = "queued"
	RenditionJobRunning          RenditionJobState = "running"
	RenditionJobRetryWait        RenditionJobState = "retry_wait"
	RenditionJobOperatorRequired RenditionJobState = "operator_required"
	RenditionJobFailed           RenditionJobState = "failed"
	RenditionJobCompleted        RenditionJobState = "completed"
)

// RenditionJobPhase records only provider-neutral durable boundaries.
type RenditionJobPhase string

const (
	RenditionPhaseQueued           RenditionJobPhase = "queued"
	RenditionPhaseProvider         RenditionJobPhase = "provider"
	RenditionPhaseBuildStaged      RenditionJobPhase = "build_staged"
	RenditionPhaseGenerationStaged RenditionJobPhase = "generation_staged"
	RenditionPhasePublished        RenditionJobPhase = "published"
)

// RenditionFailureCode is a bounded aggregate classification. It deliberately
// cannot carry provider text, paths, source bytes, or credentials.
type RenditionFailureCode string

const (
	RenditionFailureTransient      RenditionFailureCode = "transient"
	RenditionFailureTerminal       RenditionFailureCode = "terminal"
	RenditionFailureAmbiguous      RenditionFailureCode = "ambiguous"
	RenditionFailureConsent        RenditionFailureCode = "consent"
	RenditionFailureStaleAuthority RenditionFailureCode = "stale_authority"
)

var (
	ErrRenditionJobLeaseHeld        = errors.New("rendition job lease is held")
	ErrRenditionJobFenced           = errors.New("rendition job claim is fenced")
	ErrRenditionJobOperatorRequired = errors.New("rendition job requires operator resolution")
	ErrRenditionJobTerminal         = errors.New("rendition job is terminal")
	ErrRenditionJobStaleAuthority   = errors.New("rendition job authority is stale")
)

// RenditionJobErrorRetryable reports whether err is a short-lived catalog
// contention failure. Workers may retry these errors while their fenced lease
// remains live; all other catalog errors require normal state classification.
func (s *Store) RenditionJobErrorRetryable(err error) bool {
	return s != nil && err != nil && (s.driver.IsBusy(err) ||
		errors.Is(err, sql.ErrConnDone) || errors.Is(err, driver.ErrBadConn))
}

// RenditionJobRequest joins one authorized version/profile waiter to the
// immutable shared build identity.
type RenditionJobRequest struct {
	ContentVersionID       string
	Profile                ProcessingProfileRecord
	CapturedArtifactPolicy jsontext.Value
	ExecutionIdentity      document.RenditionExecutionIdentityV1
	Authorization          ProviderOperationAuthorizationRequest
}

// RenditionJob is sanitized durable status for one shared immutable build.
type RenditionJob struct {
	ID                                string
	SourceSHA256                      string
	RenditionRequestFingerprint       string
	EvidenceLexicalFingerprint        string
	CapturedArtifactPolicyFingerprint string
	ExecutionIdentityFingerprint      string
	State                             RenditionJobState
	Phase                             RenditionJobPhase
	ClaimEpoch                        int64
	FailureCode                       RenditionFailureCode
	WaiterCount                       int
	PublishedWaiterCount              int
}

// RenditionJobWaiter is one version/profile authority waiting to attach the
// shared build. Its ID is not part of build identity or artifact bytes.
type RenditionJobWaiter struct {
	ID                 string
	JobID              string
	ContentVersionID   string
	ProfileFingerprint string
	AttachmentID       string
	State              string
	FailureCode        RenditionFailureCode
}

// RenditionJobClaim is the worker-only fenced lease. ResumeHandle is opaque
// and is never included in aggregate job status.
type RenditionJobClaim struct {
	JobID        string
	Owner        string
	Epoch        int64
	LeaseExpires time.Time
	Phase        RenditionJobPhase
	ResumeHandle string
}

// RenditionJobWork is the provider-neutral immutable input needed by a worker
// after it owns a claim. It contains identities and policy, never source bytes,
// paths, provider payloads, credentials, or the opaque resume handle.
type RenditionJobWork struct {
	VaultID                string
	Job                    RenditionJob
	Waiter                 RenditionJobWaiter
	Profile                ProcessingProfileRecord
	CapturedArtifactPolicy jsontext.Value
	ExecutionIdentity      document.RenditionExecutionIdentityV1
	ExecutionSnapshot      *document.RenditionExecutionSnapshotV1
}

// EnqueueRenditionJob creates or joins the one immutable source/profile build.
func (s *Store) EnqueueRenditionJob(
	ctx context.Context, request RenditionJobRequest,
) (RenditionJob, RenditionJobWaiter, error) {
	profile, err := normalizeProcessingProfileRecord(request.Profile)
	if err != nil {
		return RenditionJob{}, RenditionJobWaiter{}, fmt.Errorf("enqueueing rendition job: %w", err)
	}
	policy, err := normalizeCapturedArtifactPolicyV1(request.CapturedArtifactPolicy)
	if err != nil {
		return RenditionJob{}, RenditionJobWaiter{}, fmt.Errorf("enqueueing rendition job: %w", err)
	}
	executionJSON, executionFingerprint, err := document.CanonicalRenditionExecutionIdentityV1(
		request.ExecutionIdentity)
	if err != nil {
		return RenditionJob{}, RenditionJobWaiter{}, fmt.Errorf(
			"enqueueing rendition job: execution identity: %w", err)
	}
	var executableProfile document.ProcessingProfileV1
	if err := json.Unmarshal(
		profile.CanonicalProfile, &executableProfile, json.RejectUnknownMembers(true)); err != nil ||
		executableProfile.Rendition == nil {
		return RenditionJob{}, RenditionJobWaiter{}, errors.New(
			"enqueueing rendition job: profile has no executable rendition binding")
	}
	if request.ExecutionIdentity.Authorization.RenditionRequestFingerprint !=
		profile.RenditionRequestFingerprint ||
		request.ExecutionIdentity.Authorization.ProviderID != executableProfile.Rendition.Descriptor.ID ||
		request.ExecutionIdentity.Authorization.DescriptorFingerprint !=
			executableProfile.Rendition.Descriptor.Fingerprint ||
		request.ExecutionIdentity.RenditionPolicy.MaxSegmentRunes !=
			executableProfile.EvidenceLexical.MaxSegmentRunes ||
		request.ExecutionIdentity.Upload.InputKind != document.RenditionInputOriginalFile {
		return RenditionJob{}, RenditionJobWaiter{}, errors.New(
			"enqueueing rendition job: execution identity does not match immutable profile")
	}
	authority, err := normalizeConsentAuthority(request.Authorization)
	if err != nil {
		return RenditionJob{}, RenditionJobWaiter{}, fmt.Errorf("enqueueing rendition job: %w", err)
	}
	if authority.profile != profile.Fingerprint ||
		authority.disclosure != profile.RenditionDisclosureFingerprint {
		return RenditionJob{}, RenditionJobWaiter{}, errors.New(
			"enqueueing rendition job: authorization does not match the exact profile disclosure")
	}
	if !slices.Equal(authority.inputs, []string{string(document.RenditionInputOriginalFile)}) {
		return RenditionJob{}, RenditionJobWaiter{}, errors.New(
			"enqueueing rendition job: authorization input classes do not match original source egress")
	}
	retainedRoles := make([]string, 0, len(policy.cardinalities))
	for role, cardinality := range policy.cardinalities {
		if cardinality.MaxCount > 0 {
			retainedRoles = append(retainedRoles, role)
		}
	}
	slices.Sort(retainedRoles)
	if !slices.Equal(authority.retained, retainedRoles) {
		return RenditionJob{}, RenditionJobWaiter{}, errors.New(
			"enqueueing rendition job: authorization retained artifact classes do not match captured policy")
	}
	if request.Authorization.PriorAuthorization != nil {
		return RenditionJob{}, RenditionJobWaiter{}, errors.New(
			"enqueueing rendition job: prior provider authorization is not a waiter identity")
	}
	if err := validateUUIDv4(request.ContentVersionID); err != nil {
		return RenditionJob{}, RenditionJobWaiter{}, fmt.Errorf(
			"enqueueing rendition job: content version: %w", ErrNotFound)
	}

	var job RenditionJob
	var waiter RenditionJobWaiter
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var sourceSHA256 string
		if err := tx.QueryRowContext(ctx,
			`SELECT blob_hash FROM content_versions WHERE version_id=?`, request.ContentVersionID,
		).Scan(&sourceSHA256); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("content version: %w", ErrNotFound)
		} else if err != nil {
			return fmt.Errorf("reading content version: %w", err)
		}
		if err := ensureProcessingProfileTx(ctx, tx, profile); err != nil {
			return err
		}
		policyFingerprint := digestCatalogJSON(policy.canonical)
		jobID := renditionSharedBuildID(s.vaultID, sourceSHA256,
			profile.RenditionRequestFingerprint, profile.EvidenceLexicalFingerprint,
			policyFingerprint, executionFingerprint)
		if request.ExecutionIdentity.Upload.SHA256 != sourceSHA256 ||
			request.ExecutionIdentity.Authorization.SourceSHA256 != sourceSHA256 {
			return errors.New("rendition execution identity does not match exact source authority")
		}
		buildSuppressed, err := derivativeBuildSuppressedTx(ctx, tx, sourceSHA256, jobID)
		if err != nil {
			return fmt.Errorf("checking rendition job build suppression: %w", err)
		}
		attachmentSuppressed, err := derivativeAttachmentSuppressedTx(
			ctx, tx, sourceSHA256, request.ContentVersionID, profile.Fingerprint, jobID)
		if err != nil {
			return fmt.Errorf("checking rendition job attachment suppression: %w", err)
		}
		if buildSuppressed || attachmentSuppressed {
			return fmt.Errorf("rendition job has active derivative purge authority: %w",
				ErrRenditionJobStaleAuthority)
		}
		nowTime := time.Now().UTC()
		now := nowTime.Format(timestampLayout)
		if _, err := authorizeProviderOperationTx(
			ctx, tx, s.vaultID, request.Authorization, nowTime); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_jobs(
				job_id,vault_uid,source_sha256,rendition_request_fingerprint,
				evidence_lexical_fingerprint,captured_artifact_policy_fingerprint,
				execution_identity_fingerprint,execution_identity_json,
				captured_artifact_policy_json,state,phase,available_at,created_at,updated_at
			) VALUES(?,?,?,?,?,?,?,?,?,'queued','queued',?,?,?)
			ON CONFLICT(vault_uid,source_sha256,rendition_request_fingerprint,
				evidence_lexical_fingerprint,captured_artifact_policy_fingerprint,
				execution_identity_fingerprint) DO NOTHING`,
			jobID, s.vaultID, sourceSHA256, profile.RenditionRequestFingerprint,
			profile.EvidenceLexicalFingerprint, policyFingerprint, executionFingerprint,
			string(executionJSON), string(policy.canonical),
			now, now, now); err != nil {
			return fmt.Errorf("creating rendition job: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET claim_epoch=max(
			claim_epoch,COALESCE((SELECT MAX(fencing_token) FROM current_rendition_roots
			WHERE root_id IN (?,?)),0)) WHERE job_id=?`,
			renditionJobRootID("build", jobID), renditionJobRootID("generation", jobID),
			jobID); err != nil {
			return fmt.Errorf("synchronizing rendition job fencing epoch: %w", err)
		}
		// A new authorization may reopen only an explicitly safe authority
		// failure. Resolve that transition before adding a waiter so terminal
		// builds cannot retain a waiter that no worker is allowed to service.
		if _, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET
			state='queued',claim_owner=NULL,lease_expires_at=NULL,available_at=?,
			provider_started=0,provider_resume_handle=NULL,selected_waiter_id=NULL,
			authorization_grant_id=NULL,authorization_incarnation_id=NULL,
			authorization_revocation_fence=NULL,failure_code=NULL,updated_at=?
			WHERE job_id=? AND state='failed'
			  AND phase IN ('queued','build_staged','generation_staged')
			  AND failure_code IN ('consent','stale_authority')`, now, now, jobID); err != nil {
			return fmt.Errorf("requeueing safe rendition authority failure: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET
			state='queued',claim_owner=NULL,lease_expires_at=NULL,available_at=?,
			provider_started=0,selected_waiter_id=NULL,authorization_grant_id=NULL,
			authorization_incarnation_id=NULL,authorization_revocation_fence=NULL,
			failure_code=NULL,updated_at=?
			WHERE job_id=? AND state='failed' AND phase='provider'
			  AND provider_resume_handle IS NOT NULL AND execution_snapshot_json IS NOT NULL
			  AND failure_code IN ('consent','stale_authority')`, now, now, jobID); err != nil {
			return fmt.Errorf("requeueing sealed rendition resume after authority failure: %w", err)
		}
		var state RenditionJobState
		if err := tx.QueryRowContext(ctx,
			`SELECT state FROM rendition_jobs WHERE job_id=?`, jobID).Scan(&state); err != nil {
			return fmt.Errorf("checking rendition job join state: %w", err)
		}
		switch state {
		case RenditionJobOperatorRequired:
			return ErrRenditionJobOperatorRequired
		case RenditionJobFailed:
			return ErrRenditionJobTerminal
		case RenditionJobQueued, RenditionJobRunning, RenditionJobRetryWait,
			RenditionJobCompleted:
		}
		waiterID := renditionScopedID("waiter", jobID, request.ContentVersionID,
			profile.Fingerprint, authority.principal, authority.scope, authority.disclosure,
			authority.inputsJSON, authority.retainedJSON)
		attachmentID := renditionScopedID("attachment", jobID, request.ContentVersionID,
			profile.Fingerprint)
		waiterResult, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_job_waiters(
				waiter_id,job_id,content_version_id,profile_fingerprint,principal,scope,
				disclosure_fingerprint,input_classes_json,retained_classes_json,state,failure_code,
				attachment_id,created_at,updated_at
			) VALUES(?,?,?,?,?,?,?,?,?,'waiting',NULL,?,?,?)
			ON CONFLICT(job_id,content_version_id,profile_fingerprint,principal,scope,
				disclosure_fingerprint,input_classes_json,retained_classes_json) DO UPDATE SET
				state='waiting',failure_code=NULL,updated_at=excluded.updated_at`,
			waiterID, jobID, request.ContentVersionID, profile.Fingerprint,
			authority.principal, authority.scope, authority.disclosure,
			authority.inputsJSON, authority.retainedJSON, attachmentID, now, now)
		if err != nil {
			return fmt.Errorf("joining rendition job: %w", err)
		}
		if _, err := waiterResult.RowsAffected(); err != nil {
			return fmt.Errorf("checking rendition job waiter: %w", err)
		}
		var alreadyActive bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
				SELECT 1 FROM rendition_heads h
				JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
				WHERE h.content_version_id=? AND h.profile_fingerprint=?
				  AND a.attachment_id=? AND a.build_id=?
			)`, request.ContentVersionID, profile.Fingerprint, attachmentID, jobID).Scan(
			&alreadyActive); err != nil {
			return fmt.Errorf("checking active rendition waiter: %w", err)
		}
		if alreadyActive {
			if _, err := tx.ExecContext(ctx, `UPDATE rendition_job_waiters
					SET state='published',failure_code=NULL,updated_at=? WHERE waiter_id=?`, now, waiterID); err != nil {
				return fmt.Errorf("joining active rendition waiter: %w", err)
			}
		} else {
			build, buildErr := loadRenditionBuild(ctx, tx, jobID)
			if buildErr != nil && !errors.Is(buildErr, ErrNotFound) {
				return fmt.Errorf("checking reusable rendition build: %w", buildErr)
			}
			if buildErr == nil && (build.SourceSHA256 != sourceSHA256 ||
				build.RenditionRequestFingerprint != profile.RenditionRequestFingerprint ||
				build.EvidenceLexicalFingerprint != profile.EvidenceLexicalFingerprint ||
				build.CapturedArtifactPolicyFingerprint != policyFingerprint) {
				return errors.New("reusable rendition build identity drifted")
			}
			if buildErr == nil {
				result, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET
					state='queued',phase='build_staged',claim_owner=NULL,lease_expires_at=NULL,
					claim_epoch=claim_epoch+1,available_at=?,provider_started=0,
					provider_resume_handle=NULL,selected_waiter_id=NULL,
					authorization_grant_id=NULL,authorization_incarnation_id=NULL,
					authorization_revocation_fence=NULL,lexical_generation_id=NULL,
					failure_code=NULL,updated_at=?
					WHERE job_id=? AND state IN ('queued','completed')`, now, now, jobID)
				if err != nil {
					return fmt.Errorf("reusing completed rendition build for waiter: %w", err)
				}
				reused, err := result.RowsAffected()
				if err != nil {
					return fmt.Errorf("checking reusable rendition job transition: %w", err)
				}
				if reused != 0 {
					var rootEpoch int64
					if err := tx.QueryRowContext(ctx,
						`SELECT claim_epoch FROM rendition_jobs WHERE job_id=?`, jobID,
					).Scan(&rootEpoch); err != nil {
						return fmt.Errorf("reading reusable rendition job epoch: %w", err)
					}
					if err := putCurrentRenditionRootTx(ctx, tx, CurrentRenditionRoot{
						ID: renditionJobRootID("build", jobID), Kind: RenditionRootJob,
						TargetKind: RenditionRootBuild, TargetID: jobID,
						FencingToken: rootEpoch, RecordedAt: now,
					}); err != nil {
						return fmt.Errorf("rooting reusable rendition build: %w", err)
					}
				}
			} else {
				if _, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET
					state='queued',phase='queued',claim_owner=NULL,lease_expires_at=NULL,
					claim_epoch=claim_epoch+1,available_at=?,provider_started=0,
					provider_resume_handle=NULL,selected_waiter_id=NULL,
					authorization_grant_id=NULL,authorization_incarnation_id=NULL,
					authorization_revocation_fence=NULL,lexical_generation_id=NULL,
					failure_code=NULL,updated_at=? WHERE job_id=? AND state='completed'`,
					now, now, jobID); err != nil {
					return fmt.Errorf("restarting collected completed rendition job: %w", err)
				}
			}
		}
		var readErr error
		job, readErr = loadRenditionJobTx(ctx, tx, jobID)
		if readErr != nil {
			return readErr
		}
		waiter, readErr = loadRenditionJobWaiterTx(ctx, tx, waiterID)
		return readErr
	})
	if err != nil {
		return RenditionJob{}, RenditionJobWaiter{}, fmt.Errorf("enqueueing rendition job: %w", err)
	}
	return job, waiter, nil
}

// RenditionJobByID returns bounded aggregate status without continuation
// handles, consent details, filenames, provider payloads, or source contents.
func (s *Store) RenditionJobByID(ctx context.Context, id string) (RenditionJob, error) {
	if err := validateCatalogSHA256(id, "rendition job ID"); err != nil {
		return RenditionJob{}, fmt.Errorf("rendition job: %w", ErrNotFound)
	}
	job, err := loadRenditionJobTx(ctx, s.db, id)
	if err != nil {
		return RenditionJob{}, fmt.Errorf("rendition job %s: %w", id, err)
	}
	return job, nil
}

func (s *Store) RenditionJobWaiterByID(ctx context.Context, id string) (RenditionJobWaiter, error) {
	if err := validateCatalogSHA256(id, "rendition waiter ID"); err != nil {
		return RenditionJobWaiter{}, fmt.Errorf("rendition waiter: %w", ErrNotFound)
	}
	waiter, err := loadRenditionJobWaiterTx(ctx, s.db, id)
	if err != nil {
		return RenditionJobWaiter{}, fmt.Errorf("rendition waiter %s: %w", id, err)
	}
	return waiter, nil
}

// ClaimRenditionJob claims or reclaims one exact job. An expired provider
// phase without a durable provider handle is atomically quarantined for
// operator resolution instead of being resubmitted.
func (s *Store) ClaimRenditionJob(
	ctx context.Context, jobID, owner string, at time.Time, lease time.Duration,
) (RenditionJobClaim, error) {
	if err := validateCatalogSHA256(jobID, "rendition job ID"); err != nil {
		return RenditionJobClaim{}, err
	}
	if !validRenditionWorkerOwner(owner) {
		return RenditionJobClaim{}, errors.New("rendition worker owner is invalid")
	}
	if lease < time.Second || lease > time.Hour {
		return RenditionJobClaim{}, errors.New("rendition job lease must be between one second and one hour")
	}
	at = at.UTC()
	expires := at.Add(lease)
	var claim RenditionJobClaim
	operatorRequired := false
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var state RenditionJobState
		var phase RenditionJobPhase
		var currentOwner, leaseRaw, availableRaw, resume, generationID sql.NullString
		var epoch int64
		var providerStarted bool
		err := tx.QueryRowContext(ctx, `
			SELECT state,phase,claim_owner,claim_epoch,lease_expires_at,available_at,
			       provider_started,provider_resume_handle,lexical_generation_id
			FROM rendition_jobs WHERE job_id=?`, jobID).Scan(
			&state, &phase, &currentOwner, &epoch, &leaseRaw, &availableRaw,
			&providerStarted, &resume, &generationID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("reading rendition job claim: %w", err)
		}
		if state == RenditionJobOperatorRequired {
			operatorRequired = true
			return nil
		}
		if state == RenditionJobCompleted || state == RenditionJobFailed {
			return ErrRenditionJobTerminal
		}
		if state == RenditionJobRunning {
			leaseExpiry, parseErr := time.Parse(timestampLayout, leaseRaw.String)
			if parseErr != nil {
				return fmt.Errorf("reading rendition job lease: %w", parseErr)
			}
			if leaseExpiry.After(at) {
				return ErrRenditionJobLeaseHeld
			}
			if phase == RenditionPhaseProvider && providerStarted && !resume.Valid {
				if _, err := tx.ExecContext(ctx, `
					UPDATE rendition_jobs SET state='operator_required',claim_owner=NULL,
					lease_expires_at=NULL,failure_code='ambiguous',updated_at=? WHERE job_id=?`,
					at.Format(timestampLayout), jobID); err != nil {
					return fmt.Errorf("quarantining ambiguous rendition job: %w", err)
				}
				operatorRequired = true
				return nil
			}
		}
		available, parseErr := time.Parse(timestampLayout, availableRaw.String)
		if parseErr != nil {
			return fmt.Errorf("reading rendition job availability: %w", parseErr)
		}
		if available.After(at) {
			return ErrRenditionJobLeaseHeld
		}
		epoch++
		result, err := tx.ExecContext(ctx, `
			UPDATE rendition_jobs SET state='running',claim_owner=?,claim_epoch=?,
				lease_expires_at=?,failure_code=NULL,updated_at=? WHERE job_id=?`,
			owner, epoch, expires.Format(timestampLayout), at.Format(timestampLayout), jobID)
		if err != nil {
			return fmt.Errorf("claiming rendition job: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return errors.Join(ErrRenditionJobFenced, err)
		}
		if phase == RenditionPhaseBuildStaged || phase == RenditionPhaseGenerationStaged {
			if err := putCurrentRenditionRootTx(ctx, tx, CurrentRenditionRoot{
				ID: renditionJobRootID("build", jobID), Kind: RenditionRootJob,
				TargetKind: RenditionRootBuild, TargetID: jobID,
				FencingToken: epoch, RecordedAt: at.Format(timestampLayout),
			}); err != nil {
				return fmt.Errorf("reclaiming rendition build root: %w", err)
			}
		}
		if phase == RenditionPhaseGenerationStaged {
			if !generationID.Valid {
				return errors.New("reclaiming rendition generation root: generation identity is missing")
			}
			if err := putCurrentRenditionRootTx(ctx, tx, CurrentRenditionRoot{
				ID: renditionJobRootID("generation", jobID), Kind: RenditionRootJob,
				TargetKind: RenditionRootLexicalGeneration, TargetID: generationID.String,
				FencingToken: epoch, RecordedAt: at.Format(timestampLayout),
			}); err != nil {
				return fmt.Errorf("reclaiming rendition generation root: %w", err)
			}
		}
		claim = RenditionJobClaim{JobID: jobID, Owner: owner, Epoch: epoch,
			LeaseExpires: expires, Phase: phase, ResumeHandle: resume.String}
		return nil
	})
	if err != nil {
		return RenditionJobClaim{}, err
	}
	if operatorRequired {
		return RenditionJobClaim{}, ErrRenditionJobOperatorRequired
	}
	return claim, nil
}

// ClaimNextRenditionJob finds and fences one ready job. Races are resolved by
// ClaimRenditionJob; an ambiguous expired candidate is quarantined and the
// scan continues without preventing unrelated work from progressing.
func (s *Store) ClaimNextRenditionJob(
	ctx context.Context, owner string, at time.Time, lease time.Duration,
) (RenditionJobClaim, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT job_id FROM rendition_jobs
		WHERE (state IN ('queued','retry_wait') AND available_at<=?)
		   OR (state='running' AND lease_expires_at<=?)
		ORDER BY available_at,job_id LIMIT 64`,
		at.UTC().Format(timestampLayout), at.UTC().Format(timestampLayout))
	if err != nil {
		return RenditionJobClaim{}, false, fmt.Errorf("listing claimable rendition jobs: %w", err)
	}
	var ids []string
	if err := func() (retErr error) {
		defer func() { retErr = errors.Join(retErr, rows.Close()) }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("reading claimable rendition job: %w", err)
			}
			ids = append(ids, id)
		}
		return rows.Err()
	}(); err != nil {
		return RenditionJobClaim{}, false, fmt.Errorf("listing claimable rendition jobs: %w", err)
	}
	for _, id := range ids {
		claim, err := s.ClaimRenditionJob(ctx, id, owner, at, lease)
		if err == nil {
			return claim, true, nil
		}
		if errors.Is(err, ErrRenditionJobLeaseHeld) ||
			errors.Is(err, ErrRenditionJobOperatorRequired) ||
			errors.Is(err, ErrRenditionJobTerminal) || errors.Is(err, ErrNotFound) {
			continue
		}
		return RenditionJobClaim{}, false, err
	}
	return RenditionJobClaim{}, false, nil
}

// RenditionJobWorkByClaim snapshots the exact shared build and selected
// waiter after checking the live lease.
func (s *Store) RenditionJobWorkByClaim(
	ctx context.Context, claim RenditionJobClaim, at time.Time,
) (RenditionJobWork, error) {
	var work RenditionJobWork
	var noAuthorizedWaiter error
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		job, err := requireRenditionClaimTx(ctx, tx, claim, at)
		if err != nil {
			return err
		}
		var waiterID sql.NullString
		var policy, executionIdentityJSON string
		var executionSnapshotJSON sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT selected_waiter_id,
			captured_artifact_policy_json,execution_identity_json,execution_snapshot_json
			FROM rendition_jobs WHERE job_id=?`, claim.JobID).Scan(
			&waiterID, &policy, &executionIdentityJSON, &executionSnapshotJSON); err != nil {
			return fmt.Errorf("reading rendition work identity: %w", err)
		}
		executionIdentity, err := document.ParseRenditionExecutionIdentityV1(
			[]byte(executionIdentityJSON))
		if err != nil {
			return fmt.Errorf("reading rendition execution identity: %w", err)
		}
		var executionSnapshot *document.RenditionExecutionSnapshotV1
		if executionSnapshotJSON.Valid {
			parsed, err := document.ParseRenditionExecutionSnapshotV1(
				[]byte(executionSnapshotJSON.String))
			if err != nil {
				return fmt.Errorf("reading rendition execution snapshot: %w", err)
			}
			executionSnapshot = &parsed
		}
		if !waiterID.Valid {
			waiterIDs, err := renditionWaitingIDsTx(ctx, tx, claim.JobID)
			if err != nil {
				return err
			}
			sawStale := false
			for _, candidateID := range waiterIDs {
				request, err := renditionWaiterAuthorizationTx(ctx, tx, job, candidateID)
				var candidateAuthorization ProviderOperationAuthorization
				if err == nil {
					candidateAuthorization, err = authorizeProviderOperationTx(
						ctx, tx, s.vaultID, request, at.UTC())
				}
				if err == nil {
					waiterID = sql.NullString{String: candidateID, Valid: true}
					if _, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET
						selected_waiter_id=?,authorization_grant_id=?,
						authorization_incarnation_id=?,authorization_revocation_fence=?,updated_at=?
						WHERE job_id=? AND claim_epoch=?`, candidateID,
						candidateAuthorization.GrantID,
						candidateAuthorization.ProcessingIncarnationID,
						candidateAuthorization.RevocationFence,
						at.UTC().Format(timestampLayout), claim.JobID, claim.Epoch); err != nil {
						return fmt.Errorf("recording rendition waiter authorization: %w", err)
					}
					break
				}
				failureCode := RenditionFailureConsent
				if errors.Is(err, ErrRenditionJobStaleAuthority) {
					sawStale = true
					failureCode = RenditionFailureStaleAuthority
				} else if !errors.Is(err, ErrProcessingConsentRequired) &&
					!errors.Is(err, ErrProcessingConsentExpired) &&
					!errors.Is(err, ErrProcessingConsentRevoked) {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE rendition_job_waiters
					SET state='rejected',failure_code=?,updated_at=? WHERE waiter_id=?`,
					failureCode, at.UTC().Format(timestampLayout), candidateID); err != nil {
					return fmt.Errorf("rejecting unauthorized rendition waiter: %w", err)
				}
			}
			if !waiterID.Valid {
				noAuthorizedWaiter = ErrProcessingConsentRequired
				if sawStale && len(waiterIDs) != 0 {
					noAuthorizedWaiter = ErrRenditionJobStaleAuthority
				}
				return nil
			}
		}
		waiter, err := loadRenditionJobWaiterTx(ctx, tx, waiterID.String)
		if err != nil {
			return err
		}
		profile, err := loadProcessingProfile(ctx, tx, waiter.ProfileFingerprint)
		if err != nil {
			return err
		}
		if _, err := renditionWaiterAuthorizationTx(ctx, tx, job, waiter.ID); err != nil {
			return err
		}
		work = RenditionJobWork{
			VaultID: s.vaultID, Job: job, Waiter: waiter, Profile: profile,
			CapturedArtifactPolicy: jsontext.Value(policy), ExecutionIdentity: executionIdentity,
			ExecutionSnapshot: executionSnapshot,
		}
		return nil
	})
	if err == nil && noAuthorizedWaiter != nil {
		return RenditionJobWork{}, noAuthorizedWaiter
	}
	return work, err
}

// RenewRenditionJobClaim extends the execution lease under the same epoch. A
// late heartbeat cannot revive an expired or stolen lease; staged artifacts
// are protected independently by durable job roots until a terminal state.
func (s *Store) RenewRenditionJobClaim(
	ctx context.Context, claim RenditionJobClaim, at time.Time, lease time.Duration,
) (RenditionJobClaim, error) {
	if lease < time.Second || lease > time.Hour {
		return RenditionJobClaim{}, errors.New("rendition job lease must be between one second and one hour")
	}
	expires := at.UTC().Add(lease)
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		job, err := requireRenditionClaimTx(ctx, tx, claim, at)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET
			lease_expires_at=?,updated_at=? WHERE job_id=? AND claim_owner=? AND claim_epoch=?`,
			expires.Format(timestampLayout), at.UTC().Format(timestampLayout),
			claim.JobID, claim.Owner, claim.Epoch)
		if err != nil {
			return fmt.Errorf("renewing rendition job claim: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return errors.Join(ErrRenditionJobFenced, err)
		}
		claim.LeaseExpires = expires
		claim.Phase = job.Phase
		return nil
	})
	return claim, err
}

// BeginRenditionProvider atomically rechecks the exact source, profile,
// consent, claim epoch, and lease immediately before provider egress.
func (s *Store) BeginRenditionProvider(
	ctx context.Context, claim RenditionJobClaim, waiterID string, at time.Time,
	snapshots ...document.RenditionExecutionSnapshotV1,
) (ProviderOperationAuthorization, error) {
	if len(snapshots) > 1 {
		return ProviderOperationAuthorization{}, errors.New(
			"beginning rendition provider accepts at most one execution snapshot")
	}
	var snapshotJSON []byte
	var snapshotFingerprint string
	var err error
	if len(snapshots) == 1 {
		snapshotJSON, err = document.CanonicalRenditionExecutionSnapshotV1(snapshots[0])
		if err != nil {
			return ProviderOperationAuthorization{}, fmt.Errorf(
				"beginning rendition provider: execution snapshot: %w", err)
		}
		_, snapshotFingerprint, err = document.CanonicalRenditionExecutionIdentityV1(
			snapshots[0].Identity)
		if err != nil {
			return ProviderOperationAuthorization{}, fmt.Errorf(
				"beginning rendition provider: execution identity: %w", err)
		}
	}
	var authorization ProviderOperationAuthorization
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		job, err := requireRenditionClaimTx(ctx, tx, claim, at)
		if err != nil {
			return err
		}
		request, err := renditionWaiterAuthorizationTx(ctx, tx, job, waiterID)
		if err != nil {
			return err
		}
		var selected, grant, incarnation, storedSnapshot, resumeHandle sql.NullString
		var fence sql.NullInt64
		var executionFingerprint string
		if err := tx.QueryRowContext(ctx, `
			SELECT selected_waiter_id,authorization_grant_id,
			       authorization_incarnation_id,authorization_revocation_fence,
			       execution_identity_fingerprint,execution_snapshot_json,
			       provider_resume_handle
			FROM rendition_jobs WHERE job_id=?`, claim.JobID).Scan(
			&selected, &grant, &incarnation, &fence, &executionFingerprint,
			&storedSnapshot, &resumeHandle); err != nil {
			return fmt.Errorf("reading rendition provider authority: %w", err)
		}
		if len(snapshotJSON) != 0 {
			if snapshotFingerprint != executionFingerprint {
				return errors.New("rendition execution snapshot does not match shared build identity")
			}
			if storedSnapshot.Valid && storedSnapshot.String != string(snapshotJSON) {
				return errors.New("rendition execution snapshot changed after provider start")
			}
		} else if !storedSnapshot.Valid || !resumeHandle.Valid {
			return errors.New("rendition provider start requires sealed execution authority")
		}
		if selected.Valid && selected.String != waiterID {
			return errors.New("rendition provider resume is bound to a different waiter authority")
		}
		if grant.Valid {
			request.PriorAuthorization = &ProviderOperationAuthorization{
				GrantID: grant.String, ProcessingIncarnationID: incarnation.String,
				RevocationFence: fence.Int64,
			}
		}
		authorization, err = authorizeProviderOperationTx(ctx, tx, s.vaultID, request, at.UTC())
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE rendition_jobs SET phase='provider',provider_started=1,
				execution_snapshot_json=COALESCE(execution_snapshot_json,?),
				selected_waiter_id=?,authorization_grant_id=?,
				authorization_incarnation_id=?,authorization_revocation_fence=?,updated_at=?
			WHERE job_id=?`, nullableBytes(snapshotJSON), waiterID, authorization.GrantID,
			authorization.ProcessingIncarnationID, authorization.RevocationFence,
			at.UTC().Format(timestampLayout), claim.JobID)
		return err
	})
	if err != nil {
		return ProviderOperationAuthorization{}, err
	}
	return authorization, nil
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}

// CheckpointRenditionProvider persists only an opaque provider-issued handle.
func (s *Store) CheckpointRenditionProvider(
	ctx context.Context, claim RenditionJobClaim, handle string, at time.Time,
) error {
	if !validRenditionResumeHandle(handle) {
		return errors.New("rendition provider resume handle is invalid")
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		job, err := requireRenditionClaimTx(ctx, tx, claim, at)
		if err != nil {
			return err
		}
		if job.Phase != RenditionPhaseProvider {
			return errors.New("rendition provider has not started")
		}
		result, err := tx.ExecContext(ctx, `UPDATE rendition_jobs
			SET provider_resume_handle=?,updated_at=? WHERE job_id=? AND claim_epoch=?`,
			handle, at.UTC().Format(timestampLayout), claim.JobID, claim.Epoch)
		if err != nil {
			return fmt.Errorf("checkpointing rendition provider: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return errors.Join(ErrRenditionJobFenced, err)
		}
		return nil
	})
}

// MarkRenditionJobRetry releases a claim after a definitive transient result.
func (s *Store) MarkRenditionJobRetry(
	ctx context.Context, claim RenditionJobClaim, code RenditionFailureCode,
	at, availableAt time.Time,
) error {
	if code != RenditionFailureTransient {
		return errors.New("rendition retry requires the transient failure class")
	}
	if availableAt.Before(at) {
		return errors.New("rendition retry availability precedes failure")
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := requireRenditionClaimTx(ctx, tx, claim, at); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET
			state='retry_wait',claim_owner=NULL,lease_expires_at=NULL,available_at=?,
			provider_started=CASE
				WHEN phase IN ('build_staged','generation_staged') THEN provider_started
				ELSE 0
			END,
			execution_snapshot_json=CASE
				WHEN phase='provider' AND provider_resume_handle IS NULL THEN NULL
				ELSE execution_snapshot_json
			END,
			failure_code=?,updated_at=? WHERE job_id=? AND claim_epoch=?`,
			availableAt.UTC().Format(timestampLayout), code, at.UTC().Format(timestampLayout),
			claim.JobID, claim.Epoch)
		if err != nil {
			return fmt.Errorf("recording rendition retry: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return errors.Join(ErrRenditionJobFenced, err)
		}
		return nil
	})
}

// MarkRenditionJobOperatorRequired quarantines an ambiguous provider outcome.
// No unbounded error text is persisted.
func (s *Store) MarkRenditionJobOperatorRequired(
	ctx context.Context, claim RenditionJobClaim, at time.Time,
) error {
	return s.finishRenditionJobClaim(
		ctx, claim, at, RenditionJobOperatorRequired, RenditionFailureAmbiguous)
}

// MarkRenditionJobFailed records a bounded terminal authority classification.
func (s *Store) MarkRenditionJobFailed(
	ctx context.Context, claim RenditionJobClaim, code RenditionFailureCode, at time.Time,
) error {
	if code != RenditionFailureTerminal && code != RenditionFailureConsent &&
		code != RenditionFailureStaleAuthority {
		return errors.New("rendition job failure class is not terminal")
	}
	return s.finishRenditionJobClaim(ctx, claim, at, RenditionJobFailed, code)
}

func (s *Store) finishRenditionJobClaim(
	ctx context.Context, claim RenditionJobClaim, at time.Time,
	state RenditionJobState, code RenditionFailureCode,
) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := requireRenditionClaimTx(ctx, tx, claim, at); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET state=?,
			claim_owner=NULL,lease_expires_at=NULL,failure_code=?,updated_at=?
			WHERE job_id=? AND claim_epoch=?`, state, code,
			at.UTC().Format(timestampLayout), claim.JobID, claim.Epoch)
		if err != nil {
			return fmt.Errorf("finishing rendition job: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return errors.Join(ErrRenditionJobFenced, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE current_rendition_roots
			SET active=0,released_at=?
			WHERE root_id IN (?,?) AND fencing_token=? AND active=1`,
			at.UTC().Format(timestampLayout), renditionJobRootID("build", claim.JobID),
			renditionJobRootID("generation", claim.JobID), claim.Epoch); err != nil {
			return fmt.Errorf("releasing terminal rendition job roots: %w", err)
		}
		return nil
	})
}

// RenditionJobPublication contains aggregate activation evidence only.
type RenditionJobPublication struct {
	JobID                string
	LexicalGenerationID  string
	PublishedWaiterCount int
	RejectedWaiterCount  int
}

// StageRenditionJobBuild atomically stages the immutable build, installs its
// epoch-fenced worker root, and advances the durable resume phase.
func (s *Store) StageRenditionJobBuild(
	ctx context.Context, claim RenditionJobClaim, record RenditionBuildRecord, at time.Time,
) error {
	normalized, err := normalizeRenditionBuildRecord(record)
	if err != nil {
		return fmt.Errorf("staging rendition job build: %w", err)
	}
	if normalized.VaultID != s.vaultID || normalized.ID != claim.JobID {
		return errors.New("staging rendition job build: build does not match claimed job identity")
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		job, err := requireRenditionClaimTx(ctx, tx, claim, at)
		if err != nil {
			return err
		}
		if job.Phase != RenditionPhaseProvider && job.Phase != RenditionPhaseBuildStaged {
			return errors.New("rendition job is not at the build staging phase")
		}
		if normalized.SourceSHA256 != job.SourceSHA256 ||
			normalized.RenditionRequestFingerprint != job.RenditionRequestFingerprint ||
			normalized.EvidenceLexicalFingerprint != job.EvidenceLexicalFingerprint ||
			normalized.CapturedArtifactPolicyFingerprint != job.CapturedArtifactPolicyFingerprint {
			return errors.New("rendition job build identity drifted")
		}
		if err := stageRenditionBuildTx(ctx, tx, normalized); err != nil {
			return err
		}
		root := CurrentRenditionRoot{
			ID: renditionJobRootID("build", claim.JobID), Kind: RenditionRootJob,
			TargetKind: RenditionRootBuild, TargetID: normalized.ID,
			FencingToken: claim.Epoch, RecordedAt: at.UTC().Format(timestampLayout),
		}
		if err := putRenditionJobRootTx(ctx, tx, root); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE rendition_jobs
			SET phase='build_staged',updated_at=? WHERE job_id=? AND claim_epoch=?`,
			at.UTC().Format(timestampLayout), claim.JobID, claim.Epoch)
		return err
	})
}

// StageRenditionJobGeneration atomically creates the complete unreachable
// lexical generation, roots it to the claim epoch, and records its identity.
func (s *Store) StageRenditionJobGeneration(
	ctx context.Context, claim RenditionJobClaim, generationID string, at time.Time,
) (LexicalGeneration, error) {
	if err := validateCatalogSHA256(generationID, "lexical generation ID"); err != nil {
		return LexicalGeneration{}, err
	}
	var generation LexicalGeneration
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		job, err := requireRenditionClaimTx(ctx, tx, claim, at)
		if err != nil {
			return err
		}
		if job.Phase != RenditionPhaseBuildStaged &&
			job.Phase != RenditionPhaseGenerationStaged {
			return errors.New("rendition job is not at the lexical generation phase")
		}
		generation, err = stageLexicalGenerationTx(ctx, tx, generationID)
		if err != nil {
			return err
		}
		root := CurrentRenditionRoot{
			ID: renditionJobRootID("generation", claim.JobID), Kind: RenditionRootJob,
			TargetKind: RenditionRootLexicalGeneration, TargetID: generation.ID,
			FencingToken: claim.Epoch, RecordedAt: at.UTC().Format(timestampLayout),
		}
		if job.Phase == RenditionPhaseGenerationStaged {
			result, err := tx.ExecContext(ctx, `UPDATE current_rendition_roots SET
				target_id=?,recorded_at=?
				WHERE root_id=? AND root_kind='job' AND target_kind='lexical_generation'
				  AND fencing_token=? AND active=1`, generation.ID, root.RecordedAt,
				root.ID, claim.Epoch)
			if err != nil {
				return fmt.Errorf("refreshing rendition job generation root: %w", err)
			}
			changed, err := result.RowsAffected()
			if err != nil || changed != 1 {
				return errors.Join(ErrRenditionJobFenced, err)
			}
		} else if err := putRenditionJobRootTx(ctx, tx, root); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE rendition_jobs SET
			phase='generation_staged',lexical_generation_id=?,updated_at=?
			WHERE job_id=? AND claim_epoch=?`, generation.ID,
			at.UTC().Format(timestampLayout), claim.JobID, claim.Epoch)
		return err
	})
	return generation, err
}

type renditionJobWaiterAuthority struct {
	waiter  RenditionJobWaiter
	profile ProcessingProfileRecord
}

// PublishRenditionJob rechecks the egress grant and every waiter inside the
// same transaction that attaches authorized waiters and flips all rendition
// and lexical heads. Revoked secondary waiters are rejected without exposing
// the successfully authorized waiters to a partial head flip.
func (s *Store) PublishRenditionJob(
	ctx context.Context, claim RenditionJobClaim, at time.Time,
) (RenditionJobPublication, error) {
	publication := RenditionJobPublication{JobID: claim.JobID}
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		job, err := requireRenditionClaimTx(ctx, tx, claim, at)
		if err != nil {
			return err
		}
		if job.Phase != RenditionPhaseGenerationStaged {
			return errors.New("rendition job is not ready for publication")
		}
		var generationID string
		var providerStarted bool
		var selectedWaiter, grantID, incarnationID sql.NullString
		var revocationFence sql.NullInt64
		if err := tx.QueryRowContext(ctx, `
			SELECT lexical_generation_id,provider_started,selected_waiter_id,authorization_grant_id,
			       authorization_incarnation_id,authorization_revocation_fence
			FROM rendition_jobs WHERE job_id=?`, claim.JobID).Scan(
			&generationID, &providerStarted, &selectedWaiter, &grantID, &incarnationID,
			&revocationFence); err != nil {
			return fmt.Errorf("reading rendition publication authority: %w", err)
		}
		if providerStarted {
			if !selectedWaiter.Valid || !grantID.Valid || !incarnationID.Valid ||
				!revocationFence.Valid {
				return errors.New("rendition publication lacks provider authorization authority")
			}
			providerRequest, err := renditionWaiterAuthorizationTx(
				ctx, tx, job, selectedWaiter.String)
			if err != nil {
				return err
			}
			providerRequest.PriorAuthorization = &ProviderOperationAuthorization{
				GrantID: grantID.String, ProcessingIncarnationID: incarnationID.String,
				RevocationFence: revocationFence.Int64,
			}
			if _, err := authorizeProviderOperationTx(
				ctx, tx, s.vaultID, providerRequest, at.UTC()); err != nil {
				return err
			}
		}

		waiterIDs, err := renditionWaitingIDsTx(ctx, tx, claim.JobID)
		if err != nil {
			return err
		}
		authorized := make([]renditionJobWaiterAuthority, 0, len(waiterIDs))
		type rejectedWaiter struct {
			id   string
			code RenditionFailureCode
		}
		rejected := make([]rejectedWaiter, 0)
		for _, waiterID := range waiterIDs {
			request, err := renditionWaiterAuthorizationTx(ctx, tx, job, waiterID)
			if err != nil {
				if errors.Is(err, ErrRenditionJobStaleAuthority) {
					rejected = append(rejected, rejectedWaiter{waiterID, RenditionFailureStaleAuthority})
					continue
				}
				return err
			}
			if _, err := authorizeProviderOperationTx(
				ctx, tx, s.vaultID, request, at.UTC()); err != nil {
				if errors.Is(err, ErrProcessingConsentRequired) ||
					errors.Is(err, ErrProcessingConsentExpired) ||
					errors.Is(err, ErrProcessingConsentRevoked) {
					rejected = append(rejected, rejectedWaiter{waiterID, RenditionFailureConsent})
					continue
				}
				return err
			}
			waiter, err := loadRenditionJobWaiterTx(ctx, tx, waiterID)
			if err != nil {
				return err
			}
			profile, err := loadProcessingProfile(ctx, tx, waiter.ProfileFingerprint)
			if err != nil {
				return err
			}
			authorized = append(authorized, renditionJobWaiterAuthority{
				waiter: waiter, profile: profile,
			})
		}
		if len(authorized) == 0 {
			return ErrProcessingConsentRequired
		}
		pairs := make([]renditionPublicationPair, 0, len(authorized))
		publishedAt := at.UTC().Format(timestampLayout)
		for _, authority := range authorized {
			attachedAt := publishedAt
			err := tx.QueryRowContext(ctx, `SELECT attached_at FROM rendition_attachments
				WHERE attachment_id=?`, authority.waiter.AttachmentID).Scan(&attachedAt)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("reading existing rendition attachment timestamp: %w", err)
			}
			attachment := RenditionAttachmentRecord{
				ID: authority.waiter.AttachmentID, VaultID: s.vaultID,
				ContentVersionID: authority.waiter.ContentVersionID,
				BuildID:          claim.JobID, Profile: authority.profile, AttachedAt: attachedAt,
			}
			head := RenditionHeadRecord{
				ContentVersionID:             authority.waiter.ContentVersionID,
				ProcessingProfileFingerprint: authority.waiter.ProfileFingerprint,
				AttachmentID:                 authority.waiter.AttachmentID, PublishedAt: publishedAt,
			}
			pairs = append(pairs, renditionPublicationPair{attachment: attachment, head: head})
		}
		if err := publishRenditionAttachmentsAndLexicalHeadsTx(
			ctx, tx, pairs, generationID); err != nil {
			return err
		}
		for _, authority := range authorized {
			if _, err := tx.ExecContext(ctx, `UPDATE rendition_job_waiters
				SET state='published',failure_code=NULL,updated_at=? WHERE waiter_id=?`,
				publishedAt, authority.waiter.ID); err != nil {
				return fmt.Errorf("publishing rendition waiter: %w", err)
			}
		}
		for _, waiter := range rejected {
			if _, err := tx.ExecContext(ctx, `UPDATE rendition_job_waiters
				SET state='rejected',failure_code=?,updated_at=? WHERE waiter_id=?`,
				waiter.code, publishedAt, waiter.id); err != nil {
				return fmt.Errorf("rejecting rendition waiter: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE rendition_jobs SET
			state='completed',phase='published',claim_owner=NULL,lease_expires_at=NULL,
			failure_code=NULL,updated_at=? WHERE job_id=? AND claim_epoch=?`,
			publishedAt, claim.JobID, claim.Epoch); err != nil {
			return fmt.Errorf("completing rendition job: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE current_rendition_roots
			SET active=0,released_at=? WHERE root_id IN (?,?) AND fencing_token=? AND active=1`,
			publishedAt, renditionJobRootID("build", claim.JobID),
			renditionJobRootID("generation", claim.JobID), claim.Epoch); err != nil {
			return fmt.Errorf("releasing rendition job roots: %w", err)
		}
		publication.LexicalGenerationID = generationID
		publication.PublishedWaiterCount = len(authorized)
		publication.RejectedWaiterCount = len(rejected)
		return nil
	})
	if err != nil {
		return RenditionJobPublication{}, err
	}
	return publication, nil
}

func renditionWaitingIDsTx(ctx context.Context, tx *sql.Tx, jobID string) (_ []string, retErr error) {
	rows, err := tx.QueryContext(ctx, `SELECT waiter_id FROM rendition_job_waiters
		WHERE job_id=? AND state='waiting' ORDER BY waiter_id`, jobID)
	if err != nil {
		return nil, fmt.Errorf("reading rendition waiters: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("reading rendition waiter: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading rendition waiters: %w", err)
	}
	return ids, nil
}

func renditionJobRootID(kind, jobID string) string {
	return "rendition_job_" + kind + "_" + jobID
}

// putRenditionJobRootTx makes a completed staging transaction safely
// replayable under the same claim. recorded_at is observation metadata, not a
// different authority; token, target, kind, and active state remain exact.
func putRenditionJobRootTx(
	ctx context.Context, tx *sql.Tx, root CurrentRenditionRoot,
) error {
	var kind CurrentRenditionRootKind
	var targetKind CurrentRenditionTargetKind
	var targetID string
	var token int64
	var active bool
	err := tx.QueryRowContext(ctx, `SELECT root_kind,target_kind,target_id,fencing_token,active
		FROM current_rendition_roots WHERE root_id=?`, root.ID).Scan(
		&kind, &targetKind, &targetID, &token, &active)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading rendition job root %s: %w", root.ID, err)
	}
	if err == nil && token == root.FencingToken && active && kind == root.Kind &&
		targetKind == root.TargetKind && targetID == root.TargetID {
		return nil
	}
	return putCurrentRenditionRootTx(ctx, tx, root)
}

func loadRenditionJobTx(ctx context.Context, query rowQuerier, id string) (RenditionJob, error) {
	var job RenditionJob
	var failure sql.NullString
	err := query.QueryRowContext(ctx, `
		SELECT j.job_id,j.source_sha256,j.rendition_request_fingerprint,
		       j.evidence_lexical_fingerprint,j.captured_artifact_policy_fingerprint,
		       j.execution_identity_fingerprint,j.state,j.phase,j.claim_epoch,j.failure_code,
		       (SELECT COUNT(*) FROM rendition_job_waiters w WHERE w.job_id=j.job_id),
		       (SELECT COUNT(*) FROM rendition_job_waiters w
		        WHERE w.job_id=j.job_id AND w.state='published')
		FROM rendition_jobs j WHERE j.job_id=?`, id).Scan(
		&job.ID, &job.SourceSHA256, &job.RenditionRequestFingerprint,
		&job.EvidenceLexicalFingerprint, &job.CapturedArtifactPolicyFingerprint,
		&job.ExecutionIdentityFingerprint, &job.State, &job.Phase, &job.ClaimEpoch, &failure,
		&job.WaiterCount, &job.PublishedWaiterCount)
	if errors.Is(err, sql.ErrNoRows) {
		return RenditionJob{}, ErrNotFound
	}
	if err != nil {
		return RenditionJob{}, fmt.Errorf("reading rendition job: %w", err)
	}
	job.FailureCode = RenditionFailureCode(failure.String)
	return job, nil
}

func loadRenditionJobWaiterTx(
	ctx context.Context, query rowQuerier, id string,
) (RenditionJobWaiter, error) {
	var waiter RenditionJobWaiter
	var failure sql.NullString
	err := query.QueryRowContext(ctx, `
		SELECT waiter_id,job_id,content_version_id,profile_fingerprint,attachment_id,state,failure_code
		FROM rendition_job_waiters WHERE waiter_id=?`, id).Scan(
		&waiter.ID, &waiter.JobID, &waiter.ContentVersionID,
		&waiter.ProfileFingerprint, &waiter.AttachmentID, &waiter.State, &failure)
	if errors.Is(err, sql.ErrNoRows) {
		return RenditionJobWaiter{}, ErrNotFound
	}
	if err != nil {
		return RenditionJobWaiter{}, fmt.Errorf("reading rendition job waiter: %w", err)
	}
	waiter.FailureCode = RenditionFailureCode(failure.String)
	return waiter, nil
}

func requireRenditionClaimTx(
	ctx context.Context, tx *sql.Tx, claim RenditionJobClaim, at time.Time,
) (RenditionJob, error) {
	if claim.JobID == "" || claim.Owner == "" || claim.Epoch <= 0 {
		return RenditionJob{}, ErrRenditionJobFenced
	}
	var owner, leaseRaw string
	var state RenditionJobState
	var epoch int64
	err := tx.QueryRowContext(ctx, `
		SELECT state,claim_owner,claim_epoch,lease_expires_at
		FROM rendition_jobs WHERE job_id=?`, claim.JobID).Scan(&state, &owner, &epoch, &leaseRaw)
	if err != nil {
		return RenditionJob{}, errors.Join(ErrRenditionJobFenced, err)
	}
	leaseExpiry, err := time.Parse(timestampLayout, leaseRaw)
	if err != nil || state != RenditionJobRunning || owner != claim.Owner ||
		epoch != claim.Epoch || !leaseExpiry.After(at.UTC()) {
		return RenditionJob{}, ErrRenditionJobFenced
	}
	job, err := loadRenditionJobTx(ctx, tx, claim.JobID)
	if err != nil {
		return RenditionJob{}, err
	}
	return job, nil
}

func renditionWaiterAuthorizationTx(
	ctx context.Context, tx *sql.Tx, job RenditionJob, waiterID string,
) (ProviderOperationAuthorizationRequest, error) {
	var request ProviderOperationAuthorizationRequest
	var inputJSON, retainedJSON, sourceSHA256, renditionFingerprint, evidenceFingerprint string
	err := tx.QueryRowContext(ctx, `
		SELECT w.principal,w.scope,w.profile_fingerprint,w.disclosure_fingerprint,
		       w.input_classes_json,w.retained_classes_json,v.blob_hash,
		       p.rendition_request_fingerprint,p.evidence_lexical_fingerprint
		FROM rendition_job_waiters w
		JOIN content_versions v ON v.version_id=w.content_version_id
		JOIN processing_profiles p ON p.profile_fingerprint=w.profile_fingerprint
		WHERE w.waiter_id=? AND w.job_id=? AND w.state='waiting'`,
		waiterID, job.ID).Scan(&request.Principal, &request.Scope,
		&request.ProfileFingerprint, &request.DisclosureFingerprint,
		&inputJSON, &retainedJSON, &sourceSHA256, &renditionFingerprint, &evidenceFingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderOperationAuthorizationRequest{}, ErrNotFound
	}
	if err != nil {
		return ProviderOperationAuthorizationRequest{}, fmt.Errorf("reading rendition waiter authority: %w", err)
	}
	if sourceSHA256 != job.SourceSHA256 || renditionFingerprint != job.RenditionRequestFingerprint ||
		evidenceFingerprint != job.EvidenceLexicalFingerprint {
		return ProviderOperationAuthorizationRequest{}, fmt.Errorf(
			"rendition job source or profile authority drifted: %w",
			ErrRenditionJobStaleAuthority)
	}
	if err := json.Unmarshal([]byte(inputJSON), &request.InputClasses); err != nil {
		return ProviderOperationAuthorizationRequest{}, fmt.Errorf("reading rendition waiter input classes: %w", err)
	}
	if err := json.Unmarshal([]byte(retainedJSON), &request.RetainedArtifactClasses); err != nil {
		return ProviderOperationAuthorizationRequest{}, fmt.Errorf("reading rendition waiter retained classes: %w", err)
	}
	return request, nil
}

func renditionSharedBuildID(
	vaultID, source, rendition, evidence, capturedPolicy, execution string,
) string {
	digest := sha256.Sum256([]byte("docbank:rendition-build:v2\x00" + vaultID + "\x00" +
		source + "\x00" + rendition + "\x00" + evidence + "\x00" + capturedPolicy +
		"\x00" + execution))
	return hex.EncodeToString(digest[:])
}

func renditionScopedID(kind string, parts ...string) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte("docbank:rendition-" + kind + ":v1"))
	for _, part := range parts {
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write([]byte(part))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func validRenditionWorkerOwner(value string) bool {
	return utf8.ValidString(value) && len(value) >= 1 && len(value) <= 128 &&
		strings.TrimSpace(value) == value
}

func validRenditionResumeHandle(value string) bool {
	if len(value) < 1 || len(value) > 512 {
		return false
	}
	for _, current := range value {
		if current > 127 || ((current < 'a' || current > 'z') &&
			(current < 'A' || current > 'Z') && (current < '0' || current > '9') && !strings.ContainsRune("-._~", current)) {
			return false
		}
	}
	return true
}
