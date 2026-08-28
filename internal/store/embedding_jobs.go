package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"time"

	"go.kenn.io/docbank/document"
)

var ErrEmbeddingJobFenced = errors.New("embedding job claim is fenced")

const embeddingJobMaxClaims = 3

type EmbeddingJobRequest struct {
	ContentVersionID string
	Profile          ProcessingProfileRecord
	BindingID        string
	Descriptor       document.EmbeddingDescriptor
	InputGeneration  EmbeddingInputGenerationRecord
	Authorization    ProviderOperationAuthorizationRequest
}

type EmbeddingJob struct{ ID string }

// EmbeddingJobStatus is the bounded provider-neutral state exposed to an
// aggregate processing service. It deliberately excludes receipts, consent
// identities, source names, and provider payloads.
type EmbeddingJobStatus struct {
	ID                 string
	ContentVersionID   string
	ProfileFingerprint string
	BindingID          string
	State              string
	FailureCode        EmbeddingFailureCode
}

type EmbeddingJobClaim struct {
	AttemptID      string
	Owner          string
	Epoch          int64
	LeaseExpiresAt time.Time
}

type EmbeddingJobWork struct {
	VaultID                   string
	ContentVersionID          string
	ProcessingProfile         ProcessingProfileRecord
	Binding                   document.EmbeddingBindingV1
	Descriptor                document.EmbeddingDescriptor
	VectorSpaceID             string
	EmbeddingInputFingerprint string
	InputGeneration           EmbeddingInputGenerationRecord
	Inputs                    []document.EmbeddingInput
	Consent                   ProviderOperationAuthorizationRequest
	SourceBlobHash            string
	SourceBytes               int64
	SourceFilename            string
	SourceMediaType           string
}

type EmbeddingAttemptReceipt struct {
	AttemptID           string
	ProviderFingerprint string
	ProfileFingerprint  string
	BindingID           string
	InputKind           document.EmbeddingInputKind
	Rows                int
	Dimensions          int
	ProviderCalls       int
	Retries             int
	Elapsed             time.Duration
	FailureCode         string
}

func (s *Store) EnqueueEmbeddingJob(ctx context.Context, request EmbeddingJobRequest) (EmbeddingJob, error) {
	profile, err := normalizeProcessingProfileRecord(request.Profile)
	if err != nil {
		return EmbeddingJob{}, fmt.Errorf("enqueueing embedding job: %w", err)
	}
	descriptor, err := document.NewEmbeddingDescriptor(request.Descriptor)
	if err != nil || !reflect.DeepEqual(descriptor, request.Descriptor) {
		return EmbeddingJob{}, errors.New("enqueueing embedding job: descriptor is not canonical")
	}
	binding, fingerprints, err := embeddingBindingFromProfile(profile, request.BindingID)
	if err != nil {
		return EmbeddingJob{}, err
	}
	if binding.Descriptor.Fingerprint != descriptor.Fingerprint || binding.Descriptor.ID != descriptor.ID ||
		request.InputGeneration.SourceVersionID != request.ContentVersionID ||
		request.InputGeneration.ProcessingProfileFingerprint != profile.Fingerprint {
		return EmbeddingJob{}, errors.New("enqueueing embedding job: immutable authority does not match binding")
	}
	authority, err := normalizeConsentAuthority(request.Authorization)
	if err != nil || authority.profile != profile.Fingerprint || authority.disclosure != binding.DisclosureFingerprint {
		return EmbeddingJob{}, errors.New("enqueueing embedding job: consent does not match binding")
	}
	space := EmbeddingVectorSpaceRecord{ID: fingerprints.VectorSpace[binding.Name],
		ContractVersion: EmbeddingVectorSpaceContractV1, Descriptor: descriptor,
		ProviderDescriptor: descriptor.ID, ProviderRevision: descriptor.ModelRevision,
		DescriptorFingerprint: descriptor.Fingerprint, CompatibilityID: descriptor.CompatibilityID,
		Dimensions: descriptor.Dimension, Metric: descriptor.Metric, Normalization: descriptor.Normalization,
		ScalarEncoding: descriptor.ScalarEncoding, DocumentFormatter: descriptor.DocumentFormatter,
		QueryFormatter: descriptor.QueryFormatter, ModelInputFingerprint: descriptor.ModelInput.Fingerprint}
	if binding.InputKind == document.EmbeddingInputRenditionChunk {
		if len(request.InputGeneration.GenerationJSON) == 0 {
			return EmbeddingJob{}, errors.New("enqueueing embedding job: exact E2 generation is required")
		}
		record := EmbeddingSetRecord{BindingID: binding.Name, InputKind: binding.InputKind,
			ProcessingProfileFingerprint: profile.Fingerprint,
			EmbeddingInputFingerprint:    fingerprints.EmbeddingInput[binding.Name],
			VectorSpace:                  space, InputGeneration: request.InputGeneration}
		if err := validateEmbeddingBindingAuthority(record, binding, fingerprints); err != nil {
			return EmbeddingJob{}, fmt.Errorf("enqueueing embedding job: %w", err)
		}
	}
	now := time.Now().UTC().Format(timestampLayout)
	generationProjection := request.InputGeneration
	generationProjection.GenerationJSON = nil
	jobID := embeddingJobID(s.vaultID, request.ContentVersionID, profile.Fingerprint,
		binding.Name, binding.InputKind, request.InputGeneration.ID)
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := ensureProcessingProfileTx(ctx, tx, profile); err != nil {
			return err
		}
		if err := validateEmbeddingInputGeneration(request.InputGeneration); err != nil {
			return err
		}
		if err := insertVectorSpaceTx(ctx, tx, space); err != nil {
			return err
		}
		if err := validateEmbeddingJobGenerationTx(ctx, tx, binding, request.InputGeneration); err != nil {
			return err
		}
		if err := insertInputGenerationTx(ctx, tx, generationProjection); err != nil {
			return err
		}
		if _, err := authorizeProviderOperationTx(ctx, tx, s.vaultID, request.Authorization, time.Now().UTC()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO embedding_jobs(
			job_id,vault_uid,content_version_id,profile_fingerprint,binding_id,input_kind,
			generation_id,vector_space_id,principal,scope,state,available_at,created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,'queued',?,?,?) ON CONFLICT(job_id) DO NOTHING`,
			jobID, s.vaultID, request.ContentVersionID, profile.Fingerprint, binding.Name,
			binding.InputKind, request.InputGeneration.ID, space.ID, authority.principal,
			authority.scope, now, now, now)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE embedding_jobs SET claim_epoch=max(claim_epoch,
			COALESCE((SELECT MAX(fencing_token) FROM current_rendition_roots WHERE root_id=?),0))
			WHERE job_id=?`, jobID, jobID)
		return err
	})
	return EmbeddingJob{ID: jobID}, err
}

func (s *Store) EmbeddingJobByID(ctx context.Context, id string) (EmbeddingJobStatus, error) {
	if err := validateCatalogSHA256(id, "embedding job ID"); err != nil {
		return EmbeddingJobStatus{}, fmt.Errorf("embedding job: %w", ErrNotFound)
	}
	var status EmbeddingJobStatus
	var failure sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT job_id,content_version_id,profile_fingerprint,binding_id,state,failure_code
		FROM embedding_jobs WHERE job_id=?`, id).Scan(&status.ID, &status.ContentVersionID,
		&status.ProfileFingerprint, &status.BindingID, &status.State, &failure)
	if errors.Is(err, sql.ErrNoRows) {
		return EmbeddingJobStatus{}, ErrNotFound
	}
	if err != nil {
		return EmbeddingJobStatus{}, fmt.Errorf("reading embedding job status: %w", err)
	}
	if failure.Valid {
		status.FailureCode = EmbeddingFailureCode(failure.String)
	}
	return status, nil
}

func (s *Store) EmbeddingJobsForVersionProfile(ctx context.Context, versionID,
	profileFingerprint string,
) ([]EmbeddingJobStatus, error) {
	if err := validateUUIDv4(versionID); err != nil {
		return nil, ErrNotFound
	}
	if err := validateCatalogSHA256(profileFingerprint, "processing profile fingerprint"); err != nil {
		return nil, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT job_id,content_version_id,profile_fingerprint,binding_id,state,failure_code
		FROM embedding_jobs WHERE content_version_id=? AND profile_fingerprint=? ORDER BY binding_id,job_id`,
		versionID, profileFingerprint)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []EmbeddingJobStatus
	for rows.Next() {
		var status EmbeddingJobStatus
		var failure sql.NullString
		if err := rows.Scan(&status.ID, &status.ContentVersionID, &status.ProfileFingerprint,
			&status.BindingID, &status.State, &failure); err != nil {
			return nil, err
		}
		if failure.Valid {
			status.FailureCode = EmbeddingFailureCode(failure.String)
		}
		result = append(result, status)
	}
	return result, rows.Err()
}

func (s *Store) ClaimNextEmbeddingWork(ctx context.Context, owner string, at time.Time, lease time.Duration) (EmbeddingJobClaim, EmbeddingJobWork, bool, error) {
	if !validRenditionWorkerOwner(owner) || at.IsZero() || lease <= 0 {
		return EmbeddingJobClaim{}, EmbeddingJobWork{}, false, errors.New("embedding claim is invalid")
	}
	var claim EmbeddingJobClaim
	var work EmbeddingJobWork
	found := false
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var jobID string
		err := tx.QueryRowContext(ctx, `SELECT j.job_id FROM embedding_jobs j
			WHERE j.state IN ('queued','retry_wait','running') AND j.available_at<=?
			  AND NOT EXISTS(SELECT 1 FROM embedding_heads h
			    JOIN embedding_sets s ON s.embedding_set_id=h.embedding_set_id
			    WHERE h.content_version_id=j.content_version_id AND h.binding_id=j.binding_id
			      AND h.input_kind=j.input_kind AND s.profile_fingerprint=j.profile_fingerprint
			      AND s.input_generation_id=j.generation_id AND s.vector_space_id=j.vector_space_id)
			  AND NOT EXISTS(SELECT 1 FROM current_rendition_roots r WHERE r.root_id=j.job_id
			    AND r.active=1 AND r.expires_at>?)
			ORDER BY j.available_at,j.job_id LIMIT 1`, at.UTC().Format(timestampLayout), at.UTC().Format(timestampLayout)).Scan(&jobID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		claim, work, err = claimEmbeddingJobTx(ctx, tx, s.vaultID, jobID, owner, at, lease)
		if err != nil {
			return err
		}
		found = true
		return nil
	})
	return claim, work, found, err
}

// ClaimEmbeddingWork fences one exact ready embedding job. It never consumes
// another binding's queued work merely because that job sorts first.
func (s *Store) ClaimEmbeddingWork(ctx context.Context, jobID, owner string, at time.Time,
	lease time.Duration,
) (EmbeddingJobClaim, EmbeddingJobWork, bool, error) {
	if err := validateCatalogSHA256(jobID, "embedding job ID"); err != nil ||
		!validRenditionWorkerOwner(owner) || at.IsZero() || lease <= 0 {
		return EmbeddingJobClaim{}, EmbeddingJobWork{}, false, errors.New("embedding claim is invalid")
	}
	var claim EmbeddingJobClaim
	var work EmbeddingJobWork
	found := false
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var eligible bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM embedding_jobs j
			WHERE j.job_id=? AND j.state IN ('queued','retry_wait','running') AND j.available_at<=?
			  AND NOT EXISTS(SELECT 1 FROM embedding_heads h
			    JOIN embedding_sets s ON s.embedding_set_id=h.embedding_set_id
			    WHERE h.content_version_id=j.content_version_id AND h.binding_id=j.binding_id
			      AND h.input_kind=j.input_kind AND s.profile_fingerprint=j.profile_fingerprint
			      AND s.input_generation_id=j.generation_id AND s.vector_space_id=j.vector_space_id)
			  AND NOT EXISTS(SELECT 1 FROM current_rendition_roots r WHERE r.root_id=j.job_id
			    AND r.active=1 AND r.expires_at>?))`, jobID, at.UTC().Format(timestampLayout),
			at.UTC().Format(timestampLayout)).Scan(&eligible); err != nil {
			return err
		}
		if !eligible {
			return nil
		}
		var err error
		claim, work, err = claimEmbeddingJobTx(ctx, tx, s.vaultID, jobID, owner, at, lease)
		found = err == nil
		return err
	})
	return claim, work, found, err
}

func claimEmbeddingJobTx(ctx context.Context, tx *sql.Tx, vaultID, jobID, owner string,
	at time.Time, lease time.Duration,
) (EmbeddingJobClaim, EmbeddingJobWork, error) {
	var epoch int64
	if err := tx.QueryRowContext(ctx, `SELECT claim_epoch+1 FROM embedding_jobs WHERE job_id=?`, jobID).Scan(&epoch); err != nil {
		return EmbeddingJobClaim{}, EmbeddingJobWork{}, err
	}
	expires := at.UTC().Add(lease)
	if _, err := tx.ExecContext(ctx, `UPDATE embedding_jobs SET state='running',claim_owner=?,claim_epoch=?,lease_expires_at=?,updated_at=? WHERE job_id=?`,
		owner, epoch, expires.Format(timestampLayout), at.UTC().Format(timestampLayout), jobID); err != nil {
		return EmbeddingJobClaim{}, EmbeddingJobWork{}, err
	}
	work, err := loadEmbeddingJobWorkTx(ctx, tx, vaultID, jobID)
	if err != nil {
		return EmbeddingJobClaim{}, EmbeddingJobWork{}, err
	}
	root := CurrentRenditionRoot{ID: jobID, Kind: RenditionRootWorkerLease,
		TargetKind: RenditionRootEmbeddingGeneration, TargetID: work.InputGeneration.ID,
		FencingToken: epoch, RecordedAt: at.UTC().Format(timestampLayout), ExpiresAt: expires.Format(timestampLayout)}
	if err := putCurrentRenditionRootTx(ctx, tx, root); err != nil {
		return EmbeddingJobClaim{}, EmbeddingJobWork{}, err
	}
	return EmbeddingJobClaim{AttemptID: jobID, Owner: owner, Epoch: epoch, LeaseExpiresAt: expires}, work, nil
}

func (s *Store) RenewEmbeddingWork(ctx context.Context, claim EmbeddingJobClaim, at time.Time, lease time.Duration) (EmbeddingJobClaim, error) {
	expires := at.UTC().Add(lease)
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE embedding_jobs SET lease_expires_at=?,updated_at=?
			WHERE job_id=? AND state='running' AND claim_owner=? AND claim_epoch=? AND lease_expires_at>?`, expires.Format(timestampLayout),
			at.UTC().Format(timestampLayout), claim.AttemptID, claim.Owner, claim.Epoch, at.UTC().Format(timestampLayout))
		if err != nil {
			return err
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			return ErrEmbeddingJobFenced
		}
		result, err = tx.ExecContext(ctx, `UPDATE current_rendition_roots SET expires_at=?,recorded_at=?
			WHERE root_id=? AND root_kind=? AND fencing_token=? AND active=1 AND expires_at>?`, expires.Format(timestampLayout),
			at.UTC().Format(timestampLayout), claim.AttemptID, RenditionRootWorkerLease, claim.Epoch,
			at.UTC().Format(timestampLayout))
		if err != nil {
			return err
		}
		count, _ = result.RowsAffected()
		if count != 1 {
			return ErrEmbeddingJobFenced
		}
		return nil
	})
	claim.LeaseExpiresAt = expires
	return claim, err
}

func (s *Store) ValidateEmbeddingWork(ctx context.Context, claim EmbeddingJobClaim, work EmbeddingJobWork, at time.Time) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := requireEmbeddingWorkerLeaseTx(ctx, tx, claim.AttemptID, claim.Epoch, work.InputGeneration.ID, at); err != nil {
			return ErrEmbeddingJobFenced
		}
		current, err := loadEmbeddingJobWorkTx(ctx, tx, s.vaultID, claim.AttemptID)
		if err != nil || !reflect.DeepEqual(current, work) {
			return ErrEmbeddingJobFenced
		}
		var eligible bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM content_versions v
			JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id AND n.trashed_at IS NULL
			WHERE v.version_id=?)`, work.ContentVersionID).Scan(&eligible); err != nil || !eligible {
			return ErrEmbeddingJobFenced
		}
		eligible, err = embeddingGenerationCurrentTx(ctx, tx, work.InputGeneration,
			work.ProcessingProfile.Fingerprint)
		if err != nil || !eligible {
			return ErrEmbeddingJobFenced
		}
		return nil
	})
}

func (s *Store) ReauthorizeEmbeddingWork(ctx context.Context, claim EmbeddingJobClaim, work EmbeddingJobWork, prior *ProviderOperationAuthorization, at time.Time) (ProviderOperationAuthorization, error) {
	if err := s.ValidateEmbeddingWork(ctx, claim, work, at); err != nil {
		return ProviderOperationAuthorization{}, err
	}
	request := work.Consent
	request.PriorAuthorization = prior
	return s.AuthorizeProviderOperation(ctx, request)
}

func (s *Store) FinishEmbeddingWork(ctx context.Context, claim EmbeddingJobClaim, receipt EmbeddingAttemptReceipt, at time.Time) error {
	return s.finishEmbeddingWork(ctx, claim, EmbeddingJobWork{}, "completed", "", receipt, at, at)
}

func (s *Store) FailEmbeddingWork(ctx context.Context, claim EmbeddingJobClaim, work EmbeddingJobWork, code EmbeddingFailureCode, receipt EmbeddingAttemptReceipt, at time.Time) error {
	state, available := "failed", at.UTC()
	if code == EmbeddingFailureProviderUnavailable && claim.Epoch < embeddingJobMaxClaims {
		state, available = "retry_wait", at.UTC().Add(time.Minute)
	}
	return s.finishEmbeddingWork(ctx, claim, work, state, code, receipt, at, available)
}

func (s *Store) finishEmbeddingWork(ctx context.Context, claim EmbeddingJobClaim, work EmbeddingJobWork, state string,
	code EmbeddingFailureCode, receipt EmbeddingAttemptReceipt, at, available time.Time,
) error {
	encoded, err := json.Marshal(struct {
		AttemptID, ProviderFingerprint, ProfileFingerprint, BindingID, InputKind, FailureCode string
		Rows, Dimensions, ProviderCalls, Retries                                              int
		ElapsedNanoseconds                                                                    int64
	}{receipt.AttemptID, receipt.ProviderFingerprint, receipt.ProfileFingerprint, receipt.BindingID,
		string(receipt.InputKind), receipt.FailureCode, receipt.Rows, receipt.Dimensions,
		receipt.ProviderCalls, receipt.Retries, receipt.Elapsed.Nanoseconds()})
	if err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE embedding_jobs SET state=?,claim_owner=NULL,lease_expires_at=NULL,
			available_at=?,failure_code=NULLIF(?,''),receipt_json=?,updated_at=?
			WHERE job_id=? AND state='running' AND claim_owner=? AND claim_epoch=? AND lease_expires_at>?`, state,
			available.UTC().Format(timestampLayout), code, string(encoded), at.UTC().Format(timestampLayout),
			claim.AttemptID, claim.Owner, claim.Epoch, at.UTC().Format(timestampLayout))
		if err != nil {
			return err
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			return ErrEmbeddingJobFenced
		}
		if code != "" {
			_, err = tx.ExecContext(ctx, `INSERT INTO embedding_failures(content_version_id,profile_fingerprint,binding_id,input_kind,failure_code,failed_at)
				VALUES(?,?,?,?,?,?) ON CONFLICT(content_version_id,binding_id,input_kind) DO UPDATE SET
				profile_fingerprint=excluded.profile_fingerprint,failure_code=excluded.failure_code,failed_at=excluded.failed_at`,
				work.ContentVersionID, work.ProcessingProfile.Fingerprint, work.Binding.Name, work.Binding.InputKind,
				code, at.UTC().Format(timestampLayout))
			if err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE current_rendition_roots SET active=0,released_at=?
			WHERE root_id=? AND fencing_token=? AND active=1`, at.UTC().Format(timestampLayout), claim.AttemptID, claim.Epoch)
		return err
	})
}

func loadEmbeddingJobWorkTx(ctx context.Context, tx *sql.Tx, vaultID, jobID string) (EmbeddingJobWork, error) {
	var versionID, profileID, bindingID, generationID, spaceID, principal, scope string
	err := tx.QueryRowContext(ctx, `SELECT content_version_id,profile_fingerprint,binding_id,generation_id,vector_space_id,principal,scope
		FROM embedding_jobs WHERE job_id=?`, jobID).Scan(&versionID, &profileID, &bindingID, &generationID, &spaceID, &principal, &scope)
	if err != nil {
		return EmbeddingJobWork{}, err
	}
	profile, err := loadProcessingProfile(ctx, tx, profileID)
	if err != nil {
		return EmbeddingJobWork{}, err
	}
	binding, fingerprints, err := embeddingBindingFromProfile(profile, bindingID)
	if err != nil {
		return EmbeddingJobWork{}, err
	}
	generation, err := loadInputGenerationTx(ctx, tx, generationID)
	if err != nil {
		return EmbeddingJobWork{}, err
	}
	space, err := loadVectorSpaceTx(ctx, tx, spaceID)
	if err != nil {
		return EmbeddingJobWork{}, err
	}
	var sourceHash, filename, mediaType string
	var sourceBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT v.blob_hash,v.size,n.name,COALESCE(v.mime_type,'')
		FROM content_versions v JOIN nodes n ON n.id=v.node_id WHERE v.version_id=?`, versionID).
		Scan(&sourceHash, &sourceBytes, &filename, &mediaType); err != nil {
		return EmbeddingJobWork{}, err
	}
	return EmbeddingJobWork{VaultID: vaultID, ContentVersionID: versionID, ProcessingProfile: profile,
		Binding: binding, Descriptor: space.Descriptor, VectorSpaceID: spaceID,
		EmbeddingInputFingerprint: fingerprints.EmbeddingInput[binding.Name], InputGeneration: generation,
		SourceBlobHash: sourceHash, SourceBytes: sourceBytes, SourceFilename: filename, SourceMediaType: mediaType,
		Consent: ProviderOperationAuthorizationRequest{Principal: principal, Scope: scope,
			ProfileFingerprint: profileID, DisclosureFingerprint: binding.DisclosureFingerprint,
			InputClasses: []string{string(binding.InputKind)}, RetainedArtifactClasses: []string{"embedding_vector_set"}}}, nil
}

func embeddingBindingFromProfile(profile ProcessingProfileRecord, name string) (document.EmbeddingBindingV1, document.FingerprintSet, error) {
	var value document.ProcessingProfileV1
	if err := json.Unmarshal(profile.CanonicalProfile, &value); err != nil {
		return document.EmbeddingBindingV1{}, document.FingerprintSet{}, err
	}
	_, fingerprints, err := document.CanonicalProfile(value)
	if err != nil {
		return document.EmbeddingBindingV1{}, document.FingerprintSet{}, err
	}
	for _, binding := range value.Embeddings {
		if binding.Name == name {
			return binding, fingerprints, nil
		}
	}
	return document.EmbeddingBindingV1{}, document.FingerprintSet{}, ErrNotFound
}

func validateEmbeddingJobGenerationTx(ctx context.Context, tx *sql.Tx, binding document.EmbeddingBindingV1, generation EmbeddingInputGenerationRecord) error {
	var sourceHash string
	if err := tx.QueryRowContext(ctx, `SELECT blob_hash FROM content_versions WHERE version_id=?`, generation.SourceVersionID).Scan(&sourceHash); err != nil {
		return err
	}
	if binding.InputKind == document.EmbeddingInputOriginalFile {
		if generation.GenerationChecksum != sourceHash || len(generation.Inputs) != 1 ||
			generation.Inputs[0] != (EmbeddingInputReference{ID: generation.SourceVersionID, RenderedChecksum: sourceHash}) {
			return errors.New("direct embedding generation does not name exact source")
		}
		return nil
	}
	if generation.AttachmentID == "" || generation.GenerationBlobHash == "" {
		return errors.New("chunk embedding generation is not materialized")
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rendition_attachments a
		JOIN rendition_heads h ON h.content_version_id=a.content_version_id
		 AND h.profile_fingerprint=a.profile_fingerprint AND h.attachment_id=a.attachment_id
		WHERE a.attachment_id=? AND a.content_version_id=? AND a.profile_fingerprint=?`,
		generation.AttachmentID, generation.SourceVersionID, generation.ProcessingProfileFingerprint).Scan(&count); err != nil || count != 1 {
		return errors.New("chunk embedding generation attachment is stale")
	}
	return nil
}

func embeddingJobID(values ...any) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = fmt.Fprint(hash, value, "\x00")
	}
	return hex.EncodeToString(hash.Sum(nil))
}
