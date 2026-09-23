package production

import (
	"slices"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
)

// BuildProductionRenderPlan binds the ledger reservation to every sealed
// occurrence and page before the worker takes a render-stage claim.
func BuildProductionRenderPlan(job Job, finalized FinalizedProduction,
	reservation documentproduction.NumberReservation, recipe redaction.Recipe) (RenderPlan, error) {
	if finalized.Authority.Receipt == nil ||
		job.SetID != finalized.Draft.SetID || job.Revision != finalized.Draft.Revision ||
		job.ETag != finalized.Draft.ETag || job.RevisionSHA256 != finalized.Authority.Prepared.SHA256 ||
		job.PreparedInputSHA256 != finalized.Authority.Receipt.SHA256 ||
		reservation.OperationID != job.ID || reservation.RevisionSHA256 != job.RevisionSHA256 ||
		documentproduction.ValidatePreparedInputAuthority(finalized.Authority) != nil ||
		documentproduction.ValidateNumberReservation(reservation) != nil {
		return RenderPlan{}, ErrJobConflict
	}
	qualified, err := pdfproduction.QualifiedRecipeForDPI(recipe.DPI)
	if err != nil || recipe != qualified {
		return RenderPlan{}, ErrJobConflict
	}
	recipeRaw, err := canonical.Marshal(recipe)
	if err != nil || hashProductionStageBytes(recipeRaw) != finalized.Authority.Prepared.RecipeSHA256 {
		return RenderPlan{}, ErrJobConflict
	}
	plan := RenderPlan{Contract: RenderPlanContractV1, JobID: job.ID,
		RevisionSHA256: job.RevisionSHA256, Reservation: reservation}
	for _, member := range finalized.Authority.Prepared.Members {
		if member.ResolvedSHA256 != member.Resolved.SHA256 ||
			member.Member.Ordinal < 1 || member.Resolved.RecipeSHA256 != finalized.Authority.Prepared.RecipeSHA256 {
			return RenderPlan{}, ErrJobConflict
		}
		endorsed, err := PlanEndorsementPages(member.Member.ID, member.Resolved.Pages,
			member.Resolved, reservation.Numbers, recipe)
		if err != nil || len(endorsed) != len(member.Resolved.Pages) {
			return RenderPlan{}, ErrJobConflict
		}
		for index, page := range member.Resolved.Pages {
			if endorsed[index].Layout.Source != page {
				return RenderPlan{}, ErrJobConflict
			}
			plan.Pages = append(plan.Pages, RenderPagePlan{MemberID: member.Member.ID,
				MemberOrdinal: member.Member.Ordinal, Page: page.Number,
				ResolvedSHA256: member.ResolvedSHA256, Layout: endorsed[index].Layout,
				Endorsements: slices.Clone(endorsed[index].Endorsements)})
		}
	}
	plan, _, err = CanonicalRenderPlan(plan)
	return plan, err
}
