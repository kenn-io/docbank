package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"go.kenn.io/docbank/document/redaction"
	productionservice "go.kenn.io/docbank/internal/production"
)

// AssessProductionDecision resolves a proposed decision against one current
// draft snapshot without retaining it. A returned problem is a bounded
// selection outcome the caller can show before a separate mutation.
func (s *Store) AssessProductionDecision(ctx context.Context, setID string, revision, etag int64,
	decision redaction.Decision) (*redaction.Problem, error) {
	if s == nil || ctx == nil || validateUUIDv4(setID) != nil || revision < 1 || etag < 1 ||
		redaction.ValidateDecision(decision) != nil || decision.Actor != "" ||
		decision.CreatedAt != "" || decision.Revision != 0 {
		return nil, ErrInvalidProduction
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	stored, err := s.loadProductionInputsTx(ctx, tx, setID, revision)
	if err != nil {
		return nil, err
	}
	if stored.Draft.State != "draft" || stored.Draft.ETag != etag {
		return nil, ErrProductionRevisionConflict
	}
	if err := checkProductionEvidenceHeadsTx(ctx, tx, stored.Members); err != nil {
		return nil, err
	}
	var target *productionservice.StoredPreparedMember
	for index := range stored.Members {
		member := &stored.Members[index]
		for _, existing := range member.Decisions {
			if existing.ID == decision.ID && member.Member.ID != decision.MemberID {
				return nil, ErrInvalidProduction
			}
		}
		if member.Member.ID == decision.MemberID {
			target = member
		}
	}
	if target == nil {
		return nil, ErrNotFound
	}
	if target.Member.MapSHA256 != decision.Selector.MapSHA256 {
		return nil, ErrInvalidProduction
	}
	mapAuthority, found, err := loadProductionTextMapTx(ctx, tx, target.Member.MapSHA256)
	if err != nil || !found {
		return nil, errors.Join(ErrInvalidProduction, err)
	}
	recipe, err := loadProductionRecipeTx(ctx, tx, stored.Draft)
	if err != nil {
		return nil, err
	}
	decisions := slices.Clone(target.Decisions)
	replaced := false
	for index := range decisions {
		if decisions[index].ID == decision.ID {
			decisions[index] = decision
			replaced = true
			break
		}
	}
	if !replaced {
		decisions = append(decisions, decision)
	}
	_, resolveErr := redaction.Resolve(mapAuthority.Map, target.Member.Mode, decisions, recipe)
	var problem *redaction.Problem
	if resolveErr != nil {
		if !errors.As(resolveErr, &problem) ||
			(problem.Code != "selection_expansion_required" && problem.Code != "decision_conflict") {
			return nil, resolveErr
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return problem, nil
}
