package production

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
)

// RenderPlan is private job authority. Page order follows the sealed member
// order, including distinct occurrences of the same source version.
type RenderPlan struct {
	Contract           string                               `json:"contract"`
	JobID              string                               `json:"job_id"`
	RevisionSHA256     string                               `json:"revision_sha256"`
	Reservation        documentproduction.NumberReservation `json:"reservation"`
	Pages              []RenderPagePlan                     `json:"pages"`
	LayoutSHA256       string                               `json:"layout_sha256"`
	EndorsementsSHA256 string                               `json:"endorsements_sha256"`
	SHA256             string                               `json:"sha256"`
}

type RenderPagePlan struct {
	MemberID       string                  `json:"member_id"`
	MemberOrdinal  int64                   `json:"member_ordinal"`
	Page           int                     `json:"page"`
	ResolvedSHA256 string                  `json:"resolved_sha256"`
	Layout         redaction.PageLayout    `json:"layout"`
	Endorsements   []redaction.Endorsement `json:"endorsements"`
}

const MaxRenderPlanBytes = 128 << 20
const maxRenderPlanPages = 100_000
const maxRenderPlanTextBytes = 16 << 20
const maxRenderPlanEndorsements = 100_000
const RenderPlanContractV1 = "production-render-plan/v1"

// CanonicalRenderPlan validates and hashes the ordered plan without sorting
// occurrences or admitting a caller-supplied digest as authority.
func CanonicalRenderPlan(value RenderPlan) (RenderPlan, []byte, error) {
	if value.Contract != RenderPlanContractV1 || value.JobID == "" || !canonical.IsSHA256Hex(value.RevisionSHA256) ||
		documentproduction.ValidateNumberReservation(value.Reservation) != nil ||
		value.Reservation.OperationID != value.JobID || value.Reservation.RevisionSHA256 != value.RevisionSHA256 ||
		len(value.Pages) == 0 || len(value.Pages) > maxRenderPlanPages {
		return RenderPlan{}, nil, ErrJobConflict
	}
	value.Pages = slices.Clone(value.Pages)
	layouts := make([]redaction.PageLayout, len(value.Pages))
	endorsements := make([][]redaction.Endorsement, len(value.Pages))
	textBytes := 0
	endorsementCount := 0
	for index, page := range value.Pages {
		if page.MemberID == "" || page.MemberOrdinal < 1 || page.Page < 1 ||
			!canonical.IsSHA256Hex(page.ResolvedSHA256) || page.Layout.Source.Number != page.Page ||
			page.Layout.Output.Number != page.Page || len(page.Endorsements) > 128 {
			return RenderPlan{}, nil, ErrJobConflict
		}
		layouts[index] = page.Layout
		endorsements[index] = slices.Clone(page.Endorsements)
		endorsementCount += len(endorsements[index])
		if endorsementCount > maxRenderPlanEndorsements {
			return RenderPlan{}, nil, ErrJobConflict
		}
		if endorsements[index] == nil {
			endorsements[index] = []redaction.Endorsement{}
		}
		value.Pages[index].Endorsements = endorsements[index]
		for _, endorsement := range endorsements[index] {
			if len(endorsement.Text) > maxRenderPlanTextBytes-textBytes {
				return RenderPlan{}, nil, ErrJobConflict
			}
			textBytes += len(endorsement.Text)
		}
	}
	layoutRaw, err := canonical.Marshal(layouts)
	if err != nil {
		return RenderPlan{}, nil, ErrJobConflict
	}
	endorsementRaw, err := canonical.Marshal(endorsements)
	if err != nil {
		return RenderPlan{}, nil, ErrJobConflict
	}
	layoutSHA := renderPlanSHA256(layoutRaw)
	endorsementSHA := renderPlanSHA256(endorsementRaw)
	if value.LayoutSHA256 != "" && value.LayoutSHA256 != layoutSHA ||
		value.EndorsementsSHA256 != "" && value.EndorsementsSHA256 != endorsementSHA {
		return RenderPlan{}, nil, ErrJobConflict
	}
	value.LayoutSHA256, value.EndorsementsSHA256 = layoutSHA, endorsementSHA
	claimedSHA := value.SHA256
	value.SHA256 = ""
	raw, err := canonical.Marshal(value)
	if err != nil || len(raw) > MaxRenderPlanBytes {
		return RenderPlan{}, nil, ErrJobConflict
	}
	value.SHA256 = renderPlanSHA256(raw)
	if claimedSHA != "" && claimedSHA != value.SHA256 {
		return RenderPlan{}, nil, ErrJobConflict
	}
	stored, err := canonical.Marshal(value)
	if err != nil || len(stored) > MaxRenderPlanBytes {
		return RenderPlan{}, nil, ErrJobConflict
	}
	return value, stored, nil
}

func renderPlanSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
