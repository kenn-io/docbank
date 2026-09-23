package store

import (
	"context"
	"database/sql"
	"slices"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/production"
)

// validateProductionPublicationTx makes the persisted render plan and every
// page-stage receipt publication authority. A manifest cannot substitute an
// unstamped page or omit a member PDF, even if its digest is self-consistent.
func (s *Store) validateProductionPublicationTx(ctx context.Context, tx *sql.Tx, job production.Job,
	receipt documentproduction.ProductionReceipt, manifest documentproduction.ArtifactManifest,
	endorsements []redaction.Endorsement) error {
	plan, revisionSHA, preparedSHA, authority, err := loadProductionPagePlanTx(ctx, tx, job.ID)
	if err != nil || revisionSHA != job.RevisionSHA256 || preparedSHA != job.PreparedInputSHA256 ||
		plan.Reservation.SHA256 != receipt.NumberReservationSHA256 || plan.LayoutSHA256 != receipt.LayoutSHA256 ||
		authority.Prepared.Policy.SHA256 != receipt.PolicySHA256 {
		return production.ErrJobConflict
	}
	var plannedEndorsements []redaction.Endorsement
	for _, page := range plan.Pages {
		plannedEndorsements = append(plannedEndorsements, page.Endorsements...)
	}
	if len(plannedEndorsements) == 0 {
		plannedEndorsements = []redaction.Endorsement{}
	}
	if !slices.Equal(plannedEndorsements, endorsements) {
		return production.ErrJobConflict
	}
	if len(manifest.Artifacts) != len(plan.Pages)+len(authority.Prepared.Members) {
		return production.ErrJobIncomplete
	}
	byID := make(map[string]documentproduction.Artifact, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		byID[artifact.ID] = artifact
		size, err := requirePhysicalAuthorityTx(tx, artifact.SHA256)
		if err != nil || size != artifact.Size {
			return production.ErrJobIncomplete
		}
	}
	used := make(map[string]bool, len(manifest.Artifacts))
	for _, page := range plan.Pages {
		stage, err := s.loadProductionPageStageTx(ctx, tx, job.ID, page.MemberID, page.Page)
		if err != nil || byID[stage.Artifact.ID] != stage.Artifact || used[stage.Artifact.ID] {
			return production.ErrJobIncomplete
		}
		used[stage.Artifact.ID] = true
	}
	for _, member := range authority.Prepared.Members {
		found := false
		for _, artifact := range manifest.Artifacts {
			if artifact.Role == documentproduction.ArtifactRoleRedactedPDF &&
				artifact.MemberID == member.Member.ID && artifact.MemberOrdinal == member.Member.Ordinal {
				if found || used[artifact.ID] || artifact.Size < 1 || artifact.MediaType != "application/pdf" {
					return production.ErrJobIncomplete
				}
				used[artifact.ID] = true
				found = true
			}
		}
		if !found {
			return production.ErrJobIncomplete
		}
	}
	if len(used) != len(manifest.Artifacts) {
		return production.ErrJobIncomplete
	}
	return nil
}
