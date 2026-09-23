package production

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"time"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
)

var (
	ErrJobConflict     = errors.New("production job request conflicts with existing job")
	ErrJobStaleClaim   = errors.New("production job claim is stale")
	ErrJobIncomplete   = errors.New("production job publication is incomplete")
	ErrJobStageMissing = errors.New("production job page stage is missing")
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

func endorsementDigest(values []redaction.Endorsement) string {
	raw, err := canonical.Marshal(slices.Clone(values))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
