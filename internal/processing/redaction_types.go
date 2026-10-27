package processing

import (
	"errors"
	"uuid"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/redaction"
)

const (
	MaxRedactionRequestBytes  int64 = 17 << 20
	MaxRedactionReviewBytes   int64 = 15 << 20
	MaxRedactionResponseBytes int64 = 16 << 20
	MaxRedactionMaskBytes     int64 = 1 << 20
	MaxRedactionArchiveBytes  int64 = 544 << 20
)

var (
	ErrRedactionInvalidRequest     = errors.New("invalid native PDF redaction request")
	ErrRedactionInvalidPreparation = errors.New("native PDF preparation is inconsistent")
	ErrRedactionPreparationChanged = errors.New("native PDF evidence changed; prepare and review again")
	ErrRedactionReviewChanged      = errors.New("native PDF masks or review changed; resolve and review again")
)

type NativePDFSelection struct {
	NodeID int64               `json:"node_id"`
	Source document.PageSource `json:"source"`
}

type RedactionMask struct {
	ID       uuid.UUID          `json:"id" pattern:"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"`
	Selector redaction.Selector `json:"selector"`
}

type RedactionResolveRequest struct {
	Preparation RedactionPreparation `json:"preparation"`
	Masks       []RedactionMask      `json:"masks"`
}

type RedactionRenderRequest struct {
	AllocationID uuid.UUID            `json:"allocation_id,omitzero" pattern:"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"`
	Preparation  RedactionPreparation `json:"preparation"`
	Masks        []RedactionMask      `json:"masks"`
	ReviewSHA256 string               `json:"review_sha256"`
}

type RedactionReview struct {
	Contract             string             `json:"contract"`
	Selection            NativePDFSelection `json:"selection"`
	ObservedNodeRevision int64              `json:"observed_node_revision"`
	PreparationSHA256    string             `json:"preparation_sha256"`
	DecisionsSHA256      string             `json:"decisions_sha256"`
	Recipe               redaction.Recipe   `json:"recipe"`
	Resolved             redaction.Resolved `json:"resolved"`
	SHA256               string             `json:"sha256"`
}

type RedactionFileDigest struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type RedactionReceipt struct {
	Numbered             *NumberedDelivery   `json:"numbered,omitzero"`
	Contract             string              `json:"contract"`
	Selection            NativePDFSelection  `json:"selection"`
	AdmittedNodeRevision int64               `json:"admitted_node_revision"`
	PreparationSHA256    string              `json:"preparation_sha256"`
	DecisionsSHA256      string              `json:"decisions_sha256"`
	ReviewSHA256         string              `json:"review_sha256"`
	ResolvedSHA256       string              `json:"resolved_sha256"`
	Recipe               redaction.Recipe    `json:"recipe"`
	PageCount            int                 `json:"page_count"`
	PDF                  RedactionFileDigest `json:"pdf"`
	Text                 RedactionFileDigest `json:"text"`
}

type RedactionArchive struct {
	Receipt RedactionReceipt `json:"receipt"`
	Size    int64            `json:"size"`
	SHA256  string           `json:"sha256"`
}
