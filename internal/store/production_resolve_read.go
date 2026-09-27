package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	productionservice "go.kenn.io/docbank/internal/production"
)

// ProductionResolvedMaskPage is one page of the server's final pixel mask for
// a draft member. ReviewBinding names the entire current member plan, not just
// the mask boxes in this response.
type ProductionResolvedMaskPage struct {
	SetID          string          `json:"set_id"`
	Revision       int64           `json:"revision"`
	ETag           int64           `json:"etag"`
	MemberID       string          `json:"member_id"`
	Page           redaction.Page  `json:"page"`
	MapSHA256      string          `json:"map_sha256"`
	RecipeSHA256   string          `json:"recipe_sha256"`
	ResolvedSHA256 string          `json:"resolved_sha256"`
	ReviewBinding  string          `json:"review_binding"`
	TotalBoxes     int             `json:"total_boxes"`
	Items          []redaction.Box `json:"items"`
	NextCursor     string          `json:"next_cursor"`
}

type productionResolvedMaskCursorV1 struct {
	Kind           string `json:"kind"`
	SetID          string `json:"set_id"`
	Revision       int64  `json:"revision"`
	ETag           int64  `json:"etag"`
	MemberID       string `json:"member_id"`
	Page           int    `json:"page"`
	ResolvedSHA256 string `json:"resolved_sha256"`
	Offset         int    `json:"offset"`
}

// ProductionResolvedMaskPage resolves retained authority in one read snapshot.
// The ETag and opaque cursor keep every page tied to the same draft plan.
func (s *Store) ProductionResolvedMaskPage(ctx context.Context, setID string, revision int64,
	memberID string, etag int64, page int, cursor string, limit int) (ProductionResolvedMaskPage, error) {
	if ctx == nil || validateUUIDv4(setID) != nil || validateUUIDv4(memberID) != nil ||
		revision < 1 || etag < 1 || page < 1 || limit < 1 || limit > redaction.MaxProductionPage || len(cursor) > 1024 {
		return ProductionResolvedMaskPage{}, ErrInvalidProduction
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ProductionResolvedMaskPage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	stored, err := s.loadProductionInputsTx(ctx, tx, setID, revision)
	if err != nil {
		return ProductionResolvedMaskPage{}, err
	}
	if stored.Draft.ETag != etag {
		return ProductionResolvedMaskPage{}, ErrProductionRevisionConflict
	}
	var target *productionservice.StoredPreparedMember
	for index := range stored.Members {
		if stored.Members[index].Member.ID == memberID {
			target = &stored.Members[index]
			break
		}
	}
	if target == nil {
		return ProductionResolvedMaskPage{}, ErrNotFound
	}
	binding, resolvedSHA256, err := productionCurrentReviewBinding(stored.Draft, target)
	if err != nil {
		return ProductionResolvedMaskPage{}, err
	}
	var pageFrame redaction.Page
	for _, candidate := range target.Resolved.Pages {
		if candidate.Number == page {
			pageFrame = candidate
			break
		}
	}
	if pageFrame.Number == 0 {
		return ProductionResolvedMaskPage{}, ErrNotFound
	}
	boxes := make([]redaction.Box, 0)
	for _, box := range target.Resolved.RedactBoxes {
		if box.Page == page {
			boxes = append(boxes, box)
		}
	}
	offset := 0
	if cursor != "" {
		raw, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
		if err != nil {
			return ProductionResolvedMaskPage{}, ErrInvalidProduction
		}
		position, err := canonical.Decode[productionResolvedMaskCursorV1](raw)
		if err != nil || position.Kind != "resolved_masks" || position.SetID != setID ||
			position.Revision != revision || position.ETag != etag || position.MemberID != memberID ||
			position.Page != page || position.ResolvedSHA256 != resolvedSHA256 ||
			position.Offset < 1 || position.Offset >= len(boxes) {
			return ProductionResolvedMaskPage{}, ErrInvalidProduction
		}
		offset = position.Offset
	}
	end := min(offset+limit, len(boxes))
	result := ProductionResolvedMaskPage{SetID: setID, Revision: revision, ETag: etag,
		MemberID: memberID, Page: pageFrame, MapSHA256: target.Member.MapSHA256,
		RecipeSHA256: stored.Draft.RecipeSHA256, ResolvedSHA256: resolvedSHA256,
		ReviewBinding: binding, TotalBoxes: len(boxes),
		Items: make([]redaction.Box, end-offset)}
	copy(result.Items, boxes[offset:end])
	if end < len(boxes) {
		raw, err := canonical.Marshal(productionResolvedMaskCursorV1{Kind: "resolved_masks", SetID: setID,
			Revision: revision, ETag: etag, MemberID: memberID, Page: page,
			ResolvedSHA256: resolvedSHA256, Offset: end})
		if err != nil {
			return ProductionResolvedMaskPage{}, err
		}
		result.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	if err := tx.Commit(); err != nil {
		return ProductionResolvedMaskPage{}, err
	}
	return result, nil
}

func productionCurrentReviewBinding(draft redaction.Draft, member *productionservice.StoredPreparedMember) (string, string, error) {
	_, decisionsSHA256, err := redaction.CanonicalDecisions(member.Decisions)
	if err != nil {
		return "", "", errors.Join(ErrInvalidProduction, err)
	}
	_, resolvedSHA256, err := redaction.CanonicalResolved(member.Resolved)
	if err != nil || member.Resolved.SHA256 != resolvedSHA256 {
		return "", "", errors.Join(ErrInvalidProduction, err)
	}
	binding, err := redaction.ReviewBinding(redaction.ReviewInput{
		SetID: draft.SetID, MemberID: member.Member.ID, VaultID: member.Member.VaultID,
		SourceVersionID: member.Member.SourceVersionID, Revision: draft.Revision,
		Ordinal: member.Member.Ordinal, NodeID: member.Member.NodeID, SourceSize: member.Member.SourceSize,
		PDFSize: member.Member.PDFSize, SourceSHA256: member.Member.SourceSHA256, PDFSHA256: member.Member.PDFSHA256,
		PageInventorySHA256: member.Member.PageInventorySHA256, MapSHA256: member.Member.MapSHA256,
		Mode: member.Member.Mode, MemberHash: draft.MemberHash, InstructionsSHA256: draft.InstructionsSHA256,
		RecipeSHA256: draft.RecipeSHA256, DecisionsSHA256: decisionsSHA256, ResolvedSHA256: resolvedSHA256,
	})
	if err != nil {
		return "", "", errors.Join(ErrInvalidProduction, err)
	}
	return binding, resolvedSHA256, nil
}
