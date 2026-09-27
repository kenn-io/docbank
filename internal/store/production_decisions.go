package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
)

func (s *Store) ProductionDecisions(ctx context.Context, setID string, revision int64, cursor string, limit int) ([]redaction.Decision, string, error) {
	return s.ProductionDecisionsFiltered(ctx, setID, revision, cursor, limit, nil)
}

// ProductionDecisionsFiltered pages decisions in member/ID order. A filter has
// its own cursor scope, and each page stays within the JSON response budget.
func (s *Store) ProductionDecisionsFiltered(ctx context.Context, setID string, revision int64,
	cursor string, limit int, uncertain *bool) ([]redaction.Decision, string, error) {
	if validateUUIDv4(setID) != nil || revision < 1 || limit < 1 || limit > redaction.MaxProductionDecisionPage {
		return nil, "", ErrInvalidProduction
	}
	kind := "decisions"
	if uncertain != nil {
		kind = "decisions_uncertain_false"
		if *uncertain {
			kind = "decisions_uncertain_true"
		}
	}
	position, err := decodeProductionListCursor(cursor, kind, setID, revision)
	if err != nil {
		return nil, "", err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := scanProductionDraft(tx.QueryRowContext(ctx, productionDraftSelect+` WHERE set_id=? AND revision=?`, setID, revision)); err != nil {
		return nil, "", err
	}
	const batchSize = 512
	const maxPageBytes = 1<<20 - 8192 // leave room for the response envelope and cursor
	items := make([]redaction.Decision, 0, limit)
	bytesUsed := 2 // JSON array delimiters
	hasMore := false
	for {
		count, err := func() (count int, batchErr error) {
			rows, err := tx.QueryContext(ctx, `SELECT revision,decision_id,member_id,actor,created_at,canonical_json FROM production_decisions
				WHERE set_id=? AND revision=? AND (member_id>? OR (member_id=? AND decision_id>?))
				ORDER BY member_id,decision_id LIMIT ?`, setID, revision, position.MemberID, position.MemberID, position.ID, batchSize)
			if err != nil {
				return 0, err
			}
			defer func() { batchErr = errors.Join(batchErr, rows.Close()) }()
			for rows.Next() {
				count++
				value, err := scanProductionDecision(rows)
				if err != nil {
					return count, err
				}
				position.MemberID, position.ID = value.MemberID, value.ID
				if uncertain != nil && value.Uncertain != *uncertain {
					continue
				}
				raw, err := canonical.Marshal(value)
				if err != nil {
					return count, err
				}
				if len(items) == limit || bytesUsed+len(raw)+1 > maxPageBytes {
					if len(items) == 0 {
						return count, ErrInvalidProduction
					}
					hasMore = true
					break
				}
				items = append(items, value)
				bytesUsed += len(raw) + 1
			}
			return count, rows.Err()
		}()
		if err != nil {
			return nil, "", err
		}
		if hasMore || count < batchSize {
			break
		}
	}
	next := ""
	if hasMore {
		last := items[len(items)-1]
		next, err = encodeProductionListCursor(productionListCursorV1{Kind: kind, SetID: setID, Revision: revision, MemberID: last.MemberID, ID: last.ID})
		if err != nil {
			return nil, "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	return items, next, nil
}

func scanProductionDecision(row interface{ Scan(dest ...any) error }) (redaction.Decision, error) {
	var decisionID, memberID, actor, createdAt string
	var revision int64
	var raw []byte
	if err := row.Scan(&revision, &decisionID, &memberID, &actor, &createdAt, &raw); err != nil {
		return redaction.Decision{}, err
	}
	value, err := canonical.Decode[redaction.Decision](raw)
	if err != nil || redaction.ValidateDecision(value) != nil || value.ID != decisionID || value.MemberID != memberID ||
		value.Actor != actor || value.CreatedAt != createdAt || value.Revision != revision {
		return redaction.Decision{}, ErrInvalidProduction
	}
	return value, nil
}

func upsertProductionDecisionTx(ctx context.Context, tx *sql.Tx, setID string, revision int64, decision redaction.Decision) error {
	if redaction.ValidateDecision(decision) != nil {
		return ErrInvalidProduction
	}
	var mapSHA256 string
	if err := tx.QueryRowContext(ctx, `SELECT map_sha256 FROM production_members WHERE set_id=? AND revision=? AND member_id=?`, setID, revision, decision.MemberID).Scan(&mapSHA256); err != nil || mapSHA256 != decision.Selector.MapSHA256 {
		return ErrInvalidProduction
	}
	var existingMemberID string
	if err := tx.QueryRowContext(ctx, `SELECT member_id FROM production_decisions WHERE set_id=? AND revision=? AND decision_id=?`, setID, revision, decision.ID).Scan(&existingMemberID); err == nil && existingMemberID != decision.MemberID {
		return ErrInvalidProduction
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	raw, err := canonical.Marshal(decision)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO production_decisions(set_id,revision,decision_id,member_id,actor,created_at,canonical_json)
		VALUES(?,?,?,?,?,?,?) ON CONFLICT(set_id,revision,decision_id) DO UPDATE SET member_id=excluded.member_id,actor=excluded.actor,created_at=excluded.created_at,canonical_json=excluded.canonical_json`,
		setID, revision, decision.ID, decision.MemberID, decision.Actor, decision.CreatedAt, raw)
	if err != nil {
		return errors.Join(ErrInvalidProduction, err)
	}
	return nil
}

func productionDecisionHash(decisions []redaction.Decision) (string, error) {
	_, digest, err := redaction.CanonicalDecisions(decisions)
	return digest, err
}

func productionDecisionHashTx(ctx context.Context, tx *sql.Tx, setID string, revision int64) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT revision,decision_id,member_id,actor,created_at,canonical_json FROM production_decisions
		WHERE set_id=? AND revision=? ORDER BY member_id,decision_id`, setID, revision)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	digest := sha256.New()
	_, _ = digest.Write([]byte{'['})
	count := 0
	var priorMemberID, priorID string
	for rows.Next() {
		decision, err := scanProductionDecision(rows)
		if err != nil || count > 0 && (decision.MemberID < priorMemberID || decision.MemberID == priorMemberID && decision.ID <= priorID) {
			return "", ErrInvalidProduction
		}
		raw, err := canonical.Marshal(decision)
		if err != nil {
			return "", err
		}
		if count > 0 {
			_, _ = digest.Write([]byte{','})
		}
		_, _ = digest.Write(raw)
		priorMemberID, priorID = decision.MemberID, decision.ID
		count++
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	_, _ = digest.Write([]byte{']'})
	return hex.EncodeToString(digest.Sum(nil)), nil
}
