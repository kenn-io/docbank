package production

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
)

func TestProductionRenderPlanBindsOrderAndRejectsOversizeText(t *testing.T) {
	const jobID = "77000000-0000-4000-8000-000000000001"
	revisionSHA := testHash("sealed revision")
	reservation := documentproduction.NumberReservation{
		Contract: documentproduction.NumberReservationContractV1, Authority: "bates-ledger/v1",
		ID: "77000000-0000-4000-8000-000000000002", OperationID: jobID,
		RevisionSHA256: revisionSHA, State: "reserved",
		Numbers: []documentproduction.AssignedNumber{{MemberID: "77000000-0000-4000-8000-000000000003", MemberOrdinal: 1, Page: 1, Text: "PLAN000001"}},
	}
	_, digest, err := documentproduction.CanonicalNumberReservation(reservation)
	require.NoError(t, err)
	reservation.SHA256 = digest
	page := redaction.Page{Number: 1, FrameSHA256: testHash("frame"), Width: 72_000, Height: 72_000, Span: redaction.Span{Start: 0, End: 1}}
	plan := RenderPlan{Contract: RenderPlanContractV1, JobID: jobID, RevisionSHA256: revisionSHA, Reservation: reservation,
		Pages: []RenderPagePlan{{MemberID: reservation.Numbers[0].MemberID, MemberOrdinal: 1, Page: 1,
			ResolvedSHA256: testHash("resolved"), Layout: redaction.PageLayout{Source: page, Output: page},
			Endorsements: []redaction.Endorsement{{Text: "PLAN000001"}}}},
	}
	canonicalPlan, raw, err := CanonicalRenderPlan(plan)
	require.NoError(t, err)
	require.Less(t, len(raw), MaxRenderPlanBytes)
	replayed, replayRaw, err := CanonicalRenderPlan(canonicalPlan)
	require.NoError(t, err)
	require.Equal(t, canonicalPlan, replayed)
	require.Equal(t, raw, replayRaw)
	changed := plan
	changed.Pages = append([]RenderPagePlan(nil), plan.Pages...)
	changed.Pages[0].Endorsements = []redaction.Endorsement{{Text: "PLAN000002"}}
	changedPlan, _, err := CanonicalRenderPlan(changed)
	require.NoError(t, err)
	require.NotEqual(t, canonicalPlan.SHA256, changedPlan.SHA256)
	plan.Pages[0].Endorsements[0].Text = strings.Repeat("X", maxRenderPlanTextBytes+1)
	_, _, err = CanonicalRenderPlan(plan)
	require.ErrorIs(t, err, ErrJobConflict)
}
