package production

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
)

var (
	ErrJobConflict   = errors.New("production job request conflicts with existing job")
	ErrJobStaleClaim = errors.New("production job claim is stale")
	ErrJobIncomplete = errors.New("production job publication is incomplete")
)

const (
	ProductionJobQueued    = "queued"
	ProductionJobRunning   = "running"
	ProductionJobSucceeded = "succeeded"
	ProductionJobFailed    = "failed"
	ProductionJobCanceled  = "canceled"
)

type JobRequest struct {
	JobID, OperationID, SetID string
	Revision, ETag            int64
	PreparedInputSHA256       string
	RevisionSHA256            string
	NumberingProfileSHA256    string
}

type Job struct {
	ID, OperationID, SetID string
	Revision, ETag         int64
	PreparedInputSHA256    string
	RevisionSHA256         string
	State                  string
	Reservation            documentproduction.NumberReservation
	Receipt                documentproduction.ProductionReceipt
	Manifest               documentproduction.ArtifactManifest
	Endorsements           []redaction.Endorsement
}

type JobClaim struct {
	JobID, Worker, Token string
	Epoch                int64
	ExpiresAt            time.Time
}

type FinalizedProduction struct {
	Draft     redaction.Draft
	Authority documentproduction.PreparedInputAuthority
}

type FinalizationRequest struct {
	Finalized                             FinalizedProduction
	OperationID                           string
	NamespaceID, SnapshotID, RecipeSHA256 string
	StartAt                               int64
}

// JobStore is the storage boundary. Implementations load authority from
// stored finalized rows; callers never supply members, pages or decisions.
type JobStore interface {
	LoadFinalizedProduction(ctx context.Context, setID string, revision int64) (FinalizedProduction, error)
	AdmitProductionJob(ctx context.Context, request JobRequest) (Job, error)
	ClaimProductionJob(ctx context.Context, jobID string, worker string, lease time.Duration) (JobClaim, error)
	ReserveProductionJobNumbers(ctx context.Context, job Job, finalized FinalizedProduction) (documentproduction.NumberReservation, error)
	PublishProductionJob(ctx context.Context, claim JobClaim, job Job, receipt documentproduction.ProductionReceipt, manifest documentproduction.ArtifactManifest, endorsements []redaction.Endorsement) (Job, error)
	CancelProductionJob(ctx context.Context, jobID string) error
}

type ClaimRenewer interface {
	RenewProductionJobClaim(ctx context.Context, claim JobClaim, lease time.Duration) (JobClaim, error)
}

type Renderer interface {
	Render(ctx context.Context, finalized FinalizedProduction, reservation documentproduction.NumberReservation, endorsements []redaction.Endorsement, emit func(documentproduction.Artifact) error) error
}

type ArtifactStager interface {
	StageProductionArtifact(ctx context.Context, claim JobClaim, artifact documentproduction.Artifact) error
}

func PlanEndorsements(finalized FinalizedProduction, reservation documentproduction.NumberReservation) ([]redaction.Endorsement, error) {
	if err := documentproduction.ValidatePreparedInputAuthority(finalized.Authority); err != nil {
		return nil, err
	}
	if err := documentproduction.ValidateNumberReservation(reservation); err != nil {
		return nil, err
	}
	text := finalized.Authority.Prepared.Policy.Output.EndorsementText
	if text == "" {
		return []redaction.Endorsement{}, nil
	}
	result := make([]redaction.Endorsement, 0, len(reservation.Numbers))
	for _, number := range reservation.Numbers {
		result = append(result, redaction.Endorsement{Kind: finalized.Authority.Prepared.Policy.Output.EndorsementKind, Text: text, FontSizeMilliPoints: 900})
		_ = number
	}
	return result, nil
}

func Run(ctx context.Context, store JobStore, request JobRequest, worker string, renderer Renderer) (Job, error) {
	return runWithRenewalInterval(ctx, store, request, worker, renderer, 30*time.Second)
}

func runWithRenewalInterval(ctx context.Context, store JobStore, request JobRequest, worker string, renderer Renderer, renewalInterval time.Duration) (Job, error) {
	if store == nil || renderer == nil {
		return Job{}, ErrJobConflict
	}
	finalized, err := store.LoadFinalizedProduction(ctx, request.SetID, request.Revision)
	if err != nil {
		return Job{}, err
	}
	if finalized.Draft.State != "finalized" || finalized.Draft.SetID != request.SetID || finalized.Draft.Revision != request.Revision || finalized.Draft.ETag != request.ETag || finalized.Authority.Prepared.SHA256 != request.RevisionSHA256 || finalized.Authority.Receipt == nil || finalized.Authority.Receipt.SHA256 != request.PreparedInputSHA256 {
		return Job{}, ErrJobConflict
	}
	if err := documentproduction.ValidatePreparedInputAuthority(finalized.Authority); err != nil {
		return Job{}, err
	}
	job, err := store.AdmitProductionJob(ctx, request)
	if err != nil {
		return Job{}, fmt.Errorf("admit job: %w", err)
	}
	claim, err := store.ClaimProductionJob(ctx, job.ID, worker, 2*time.Minute)
	if err != nil {
		return Job{}, fmt.Errorf("claim job: %w", err)
	}
	renderCtx, stopRenew := context.WithCancel(ctx)
	defer stopRenew()
	var claimMu sync.RWMutex
	renewErr := make(chan error, 1)
	var renewalLost atomic.Bool
	if renewer, ok := store.(ClaimRenewer); ok {
		go func() {
			ticker := time.NewTicker(renewalInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					claimMu.RLock()
					current := claim
					claimMu.RUnlock()
					next, e := renewer.RenewProductionJobClaim(renderCtx, current, 2*time.Minute)
					if e == nil {
						claimMu.Lock()
						claim = next
						claimMu.Unlock()
					} else {
						select {
						case renewErr <- e:
						default:
						}
						renewalLost.Store(true)
						stopRenew()
						return
					}
				case <-renderCtx.Done():
					return
				}
			}
		}()
	}
	reservation, err := store.ReserveProductionJobNumbers(ctx, job, finalized)
	if err != nil {
		return Job{}, fmt.Errorf("reserve numbers: %w", err)
	}
	job.Reservation = reservation
	endorsements, err := PlanEndorsements(finalized, reservation)
	if err != nil {
		return Job{}, fmt.Errorf("planning endorsements: %w", err)
	}
	var artifacts []documentproduction.Artifact
	stager, _ := store.(ArtifactStager)
	if err := renderer.Render(renderCtx, finalized, reservation, endorsements, func(a documentproduction.Artifact) error {
		artifacts = append(artifacts, a)
		if stager != nil {
			claimMu.RLock()
			stageClaim := claim
			claimMu.RUnlock()
			return stager.StageProductionArtifact(renderCtx, stageClaim, a)
		}
		return nil
	}); err != nil {
		if renewalLost.Load() {
			select {
			case e := <-renewErr:
				return Job{}, fmt.Errorf("renew claim: %w", e)
			default:
				return Job{}, ErrJobStaleClaim
			}
		}
		return Job{}, err
	}
	select {
	case err := <-renewErr:
		return Job{}, fmt.Errorf("renew claim: %w", err)
	default:
	}
	if len(artifacts) == 0 {
		_ = store.CancelProductionJob(context.Background(), job.ID)
		return Job{}, ErrJobIncomplete
	}
	manifest := documentproduction.ArtifactManifest{Contract: documentproduction.ArtifactManifestContractV1, Artifacts: artifacts}
	_, manifest.SHA256, err = documentproduction.CanonicalArtifactManifest(manifest)
	if err != nil {
		return Job{}, fmt.Errorf("canonical artifact manifest: %w", err)
	}
	_, reservationSHA, err := documentproduction.CanonicalNumberReservation(reservation)
	if err != nil {
		return Job{}, fmt.Errorf("canonical reservation: %w", err)
	}
	preparedSHA := finalized.Authority.Receipt.SHA256
	receipt := documentproduction.ProductionReceipt{Contract: documentproduction.ProductionReceiptContractV1, ID: job.ID, JobID: job.ID, SetID: request.SetID, Revision: request.Revision, RevisionSHA256: finalized.Authority.Prepared.SHA256, PreparedInputSHA256: preparedSHA, PolicySHA256: finalized.Authority.Prepared.Policy.SHA256, NumberReservationSHA256: reservationSHA, ArtifactManifestSHA256: manifest.SHA256, EndorsementsSHA256: endorsementDigest(endorsements), LayoutSHA256: request.NumberingProfileSHA256, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	_, receipt.SHA256, err = documentproduction.CanonicalProductionReceipt(receipt)
	if err != nil {
		return Job{}, fmt.Errorf("canonical production receipt: %w", err)
	}
	claimMu.RLock()
	finalClaim := claim
	claimMu.RUnlock()
	return store.PublishProductionJob(ctx, finalClaim, job, receipt, manifest, endorsements)
}

func endorsementDigest(values []redaction.Endorsement) string {
	raw, err := canonical.Marshal(slices.Clone(values))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
