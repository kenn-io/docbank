package production

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/kit/packstore"
)

// ProductionFinalArtifactStore retains member PDFs and reopens any staged
// artifact through its job-scoped catalog authority.
type ProductionFinalArtifactStore interface {
	StageVerifiedProductionPDF(ctx context.Context, claim JobClaim, job Job,
		member documentproduction.PreparedMember, candidate *VerifiedProductionPDF) (documentproduction.Artifact, error)
	StageVerifiedProductionText(ctx context.Context, claim JobClaim, job Job,
		member documentproduction.PreparedMember, maxBytes int64) (documentproduction.Artifact, error)
	OpenVerifiedProductionArtifact(ctx context.Context, jobID string,
		artifact documentproduction.Artifact) (packstore.VerifiedReadCloser, int64, error)
}

type ProductionFinalPublicationStore interface {
	LoadProductionRenderPlan(ctx context.Context, jobID string) (RenderPlan, error)
	LoadProductionJob(ctx context.Context, jobID string) (Job, error)
	PublishProductionJob(ctx context.Context, claim JobClaim, job Job, receipt documentproduction.ProductionReceipt,
		manifest documentproduction.ArtifactManifest, endorsements []redaction.Endorsement) (Job, error)
}

// PublishVerifiedProductionJob writes each fresh member PDF from durable page
// receipts, reopens every final artifact, then asks Store to publish under the
// fenced claim. A retry of an already published job verifies historical bytes
// and returns its immutable receipt without reserving new numbers.
func PublishVerifiedProductionJob(ctx context.Context, publisher ProductionFinalPublicationStore,
	pages ProductionPageHandleStore, artifacts ProductionFinalArtifactStore, claim JobClaim, job Job,
	finalized FinalizedProduction, plan RenderPlan, recipe redaction.Recipe) (Job, error) {
	if ctx == nil || publisher == nil || pages == nil || artifacts == nil || claim.JobID != job.ID ||
		job.ID != plan.JobID || job.RevisionSHA256 != plan.RevisionSHA256 ||
		finalized.Draft.SetID != job.SetID || finalized.Draft.Revision != job.Revision ||
		finalized.Authority.Prepared.SHA256 != job.RevisionSHA256 || finalized.Authority.Receipt == nil ||
		finalized.Authority.Receipt.SHA256 != job.PreparedInputSHA256 {
		return Job{}, ErrJobConflict
	}
	qualified, err := pdfproduction.QualifiedRecipeForDPI(recipe.DPI)
	if err != nil || recipe != qualified {
		return Job{}, ErrJobConflict
	}
	want, _, err := CanonicalRenderPlan(plan)
	if err != nil || want.SHA256 != plan.SHA256 ||
		job.Reservation.SHA256 != "" && want.Reservation.SHA256 != job.Reservation.SHA256 {
		return Job{}, ErrJobConflict
	}
	job.Reservation = plan.Reservation
	storedPlan, err := publisher.LoadProductionRenderPlan(ctx, job.ID)
	if err != nil || storedPlan.SHA256 != plan.SHA256 {
		return Job{}, errors.Join(ErrJobConflict, err)
	}
	storedJob, err := publisher.LoadProductionJob(ctx, job.ID)
	if err != nil || storedJob.ID != job.ID || storedJob.SetID != job.SetID ||
		storedJob.Revision != job.Revision || storedJob.PreparedInputSHA256 != job.PreparedInputSHA256 ||
		storedJob.RevisionSHA256 != job.RevisionSHA256 {
		return Job{}, errors.Join(ErrJobConflict, err)
	}
	if storedJob.State == ProductionJobSucceeded {
		storedJob.Reservation = plan.Reservation
		if err := verifyProductionPublication(ctx, pages, artifacts, storedJob, finalized, plan, recipe.MaxStagingBytes); err != nil {
			return Job{}, err
		}
		return storedJob, nil
	}
	var listed []documentproduction.Artifact
	for _, member := range finalized.Authority.Prepared.Members {
		if err := ctx.Err(); err != nil {
			return Job{}, err
		}
		fresh, err := WriteAndVerifyProductionMember(ctx, pages, job, plan, member, recipe)
		if err != nil {
			return Job{}, err
		}
		artifact, stageErr := artifacts.StageVerifiedProductionPDF(ctx, claim, job, member, fresh)
		closeErr := fresh.Close()
		if err := errors.Join(stageErr, closeErr); err != nil {
			return Job{}, err
		}
		if artifact.Role != documentproduction.ArtifactRoleRedactedPDF || artifact.MemberID != member.Member.ID ||
			artifact.MemberOrdinal != member.Member.Ordinal || artifact.SHA256 != fresh.SHA256 || artifact.Size != fresh.Size {
			return Job{}, ErrJobConflict
		}
		listed = append(listed, artifact)
		text, err := redaction.Text(member.Resolved)
		if err != nil || int64(len(text)) > recipe.MaxStagingBytes {
			return Job{}, errors.Join(ErrJobConflict, err)
		}
		textSum := sha256.Sum256(text)
		textArtifact, err := artifacts.StageVerifiedProductionText(ctx, claim, job, member, recipe.MaxStagingBytes)
		if err != nil {
			return Job{}, err
		}
		if textArtifact.Role != documentproduction.ArtifactRoleRedactedText ||
			textArtifact.MemberID != member.Member.ID || textArtifact.MemberOrdinal != member.Member.Ordinal ||
			textArtifact.Page != 0 || textArtifact.MediaType != "text/plain; charset=utf-8" ||
			textArtifact.SHA256 != hex.EncodeToString(textSum[:]) || textArtifact.Size != int64(len(text)) {
			return Job{}, ErrJobConflict
		}
		listed = append(listed, textArtifact)
		for _, page := range plan.Pages {
			if page.MemberID != member.Member.ID {
				continue
			}
			stage, err := pages.LoadProductionPageStage(ctx, job.ID, page.MemberID, page.Page)
			if err != nil {
				return Job{}, err
			}
			want, err := BuildProductionPageStage(job, plan, member, page, stage.Artifact)
			if err != nil || want != stage {
				return Job{}, ErrJobConflict
			}
			listed = append(listed, stage.Artifact)
		}
	}
	manifest := documentproduction.ArtifactManifest{Contract: documentproduction.ArtifactManifestContractV1, Artifacts: listed}
	_, manifest.SHA256, err = documentproduction.CanonicalArtifactManifest(manifest)
	if err != nil {
		return Job{}, err
	}
	endorsements := make([]redaction.Endorsement, 0)
	for _, page := range plan.Pages {
		endorsements = append(endorsements, page.Endorsements...)
	}
	receipt := documentproduction.ProductionReceipt{Contract: documentproduction.ProductionReceiptContractV1,
		ID: job.ID, JobID: job.ID, SetID: job.SetID, Revision: job.Revision,
		RevisionSHA256: job.RevisionSHA256, PreparedInputSHA256: job.PreparedInputSHA256,
		PolicySHA256: finalized.Authority.Prepared.Policy.SHA256, NumberReservationSHA256: plan.Reservation.SHA256,
		LayoutSHA256: plan.LayoutSHA256, EndorsementsSHA256: endorsementDigest(endorsements),
		ArtifactManifestSHA256: manifest.SHA256, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	_, receipt.SHA256, err = documentproduction.CanonicalProductionReceipt(receipt)
	if err != nil {
		return Job{}, err
	}
	if err := verifyProductionArtifacts(ctx, artifacts, job.ID, manifest, recipe.MaxStagingBytes); err != nil {
		return Job{}, err
	}
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	result, err := publisher.PublishProductionJob(ctx, claim, job, receipt, manifest, endorsements)
	if err == nil {
		return result, nil
	}
	// A lost response may follow a committed metadata transaction. The
	// historical receipt must match this exact attempt before it is accepted.
	replayed, loadErr := publisher.LoadProductionJob(ctx, job.ID)
	if loadErr == nil && replayed.State == ProductionJobSucceeded && replayed.Receipt == receipt &&
		replayed.Manifest.SHA256 == manifest.SHA256 && slices.Equal(replayed.Endorsements, endorsements) {
		return replayed, nil
	}
	return Job{}, err
}

func verifyProductionPublication(ctx context.Context, pages ProductionPageHandleStore,
	artifacts ProductionFinalArtifactStore, job Job, finalized FinalizedProduction, plan RenderPlan,
	maxBytes int64) error {
	if documentproduction.ValidateProductionReceipt(job.Receipt) != nil ||
		documentproduction.ValidateArtifactManifest(job.Manifest) != nil ||
		job.Receipt.JobID != job.ID || job.Receipt.RevisionSHA256 != job.RevisionSHA256 ||
		job.Receipt.PreparedInputSHA256 != job.PreparedInputSHA256 ||
		job.Receipt.PolicySHA256 != finalized.Authority.Prepared.Policy.SHA256 ||
		job.Receipt.NumberReservationSHA256 != plan.Reservation.SHA256 ||
		job.Receipt.LayoutSHA256 != plan.LayoutSHA256 ||
		job.Receipt.EndorsementsSHA256 != endorsementDigest(job.Endorsements) ||
		job.Receipt.ArtifactManifestSHA256 != job.Manifest.SHA256 {
		return ErrJobConflict
	}
	if len(job.Manifest.Artifacts) != len(plan.Pages)+2*len(finalized.Authority.Prepared.Members) {
		return ErrJobConflict
	}
	listed := make(map[string]documentproduction.Artifact, len(job.Manifest.Artifacts))
	for _, artifact := range job.Manifest.Artifacts {
		listed[artifact.ID] = artifact
	}
	used := make(map[string]bool, len(listed))
	for _, member := range finalized.Authority.Prepared.Members {
		foundPDF, foundText := false, false
		text, err := redaction.Text(member.Resolved)
		if err != nil || int64(len(text)) > maxBytes {
			return errors.Join(ErrJobConflict, err)
		}
		textSum := sha256.Sum256(text)
		for _, artifact := range job.Manifest.Artifacts {
			if artifact.Role == documentproduction.ArtifactRoleRedactedPDF && artifact.MemberID == member.Member.ID &&
				artifact.MemberOrdinal == member.Member.Ordinal {
				if foundPDF || used[artifact.ID] {
					return ErrJobConflict
				}
				foundPDF, used[artifact.ID] = true, true
			} else if artifact.Role == documentproduction.ArtifactRoleRedactedText && artifact.MemberID == member.Member.ID &&
				artifact.MemberOrdinal == member.Member.Ordinal {
				if foundText || used[artifact.ID] || artifact.Page != 0 || artifact.MediaType != "text/plain; charset=utf-8" ||
					artifact.SHA256 != hex.EncodeToString(textSum[:]) || artifact.Size != int64(len(text)) {
					return ErrJobConflict
				}
				foundText, used[artifact.ID] = true, true
			}
		}
		if !foundPDF || !foundText {
			return ErrJobConflict
		}
		for _, page := range plan.Pages {
			if page.MemberID != member.Member.ID {
				continue
			}
			stage, err := pages.LoadProductionPageStage(ctx, job.ID, page.MemberID, page.Page)
			if err != nil {
				return err
			}
			want, err := BuildProductionPageStage(job, plan, member, page, stage.Artifact)
			if err != nil || stage != want || listed[stage.Artifact.ID] != stage.Artifact || used[stage.Artifact.ID] {
				return ErrJobConflict
			}
			used[stage.Artifact.ID] = true
		}
	}
	if len(used) != len(listed) {
		return ErrJobConflict
	}
	return verifyProductionArtifacts(ctx, artifacts, job.ID, job.Manifest, maxBytes)
}

func verifyProductionArtifacts(ctx context.Context, artifacts ProductionFinalArtifactStore, jobID string,
	manifest documentproduction.ArtifactManifest, maxBytes int64) error {
	for _, artifact := range manifest.Artifacts {
		stream, size, err := artifacts.OpenVerifiedProductionArtifact(ctx, jobID, artifact)
		if err != nil {
			return fmt.Errorf("open production artifact: %w", err)
		}
		spool, err := SpoolProductionPDF(ctx, PinnedProductionPDF{PDFSHA256: artifact.SHA256,
			Size: size, Stream: stream}, maxBytes)
		if err != nil {
			return fmt.Errorf("verify production artifact: %w", err)
		}
		if size != artifact.Size {
			_ = spool.Close()
			return ErrJobConflict
		}
		if err := spool.Close(); err != nil {
			return err
		}
	}
	return ctx.Err()
}
