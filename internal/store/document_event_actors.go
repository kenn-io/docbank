package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// DocumentEventActorClaim is one retained actor assertion from the active
// event generation. Axis keys are empty when an undated actor is attached to
// the vault-recorded fallback.
type DocumentEventActorClaim struct {
	GenerationID, EventID, Role, ActorKey, DisplayName, Address string
	Ordinal                                                     int
	EvidenceKind, EvidenceID, ClaimBasis                        string
	AxisKey, UTCKey                                             string
	Sensitive                                                   bool
}

// DocumentEventActorClaims reads the active generation and its actor rows in
// one snapshot. It preserves actor evidence independently of the event used to
// carry the claim.
func (s *Store) DocumentEventActorClaims(
	ctx context.Context,
	contentVersionID string,
) (string, []DocumentEventActorClaim, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", nil, fmt.Errorf("starting document event actor snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var generationID string
	err = tx.QueryRowContext(ctx, `SELECT generation_id FROM document_event_heads
		WHERE content_version_id=?`, contentVersionID).Scan(&generationID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, fmt.Errorf("document event actors for version %q: %w", contentVersionID, ErrNotFound)
	}
	if err != nil {
		return "", nil, fmt.Errorf("reading document event actor generation: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `SELECT a.event_id,a.role,a.actor_key,a.display_name,a.address,a.ordinal,
		CASE WHEN a.evidence_kind='' THEN e.evidence_kind ELSE a.evidence_kind END,
		CASE WHEN a.evidence_id='' THEN e.evidence_id ELSE a.evidence_id END,
		CASE WHEN a.evidence_kind='' OR (a.evidence_kind=e.evidence_kind AND a.evidence_id=e.evidence_id)
			THEN e.claim_basis ELSE 'source_asserted' END,
		CASE WHEN a.evidence_kind='' OR (a.evidence_kind=e.evidence_kind AND a.evidence_id=e.evidence_id)
			THEN e.axis_key ELSE '' END,
		CASE WHEN a.evidence_kind='' OR (a.evidence_kind=e.evidence_kind AND a.evidence_id=e.evidence_id)
			THEN COALESCE(e.utc_key,'') ELSE '' END,
		a.sensitive
		FROM document_event_actors a
		JOIN document_events e ON e.generation_id=a.generation_id AND e.event_id=a.event_id
		WHERE a.generation_id=?
		ORDER BY e.source_key,e.date_kind,a.role,a.ordinal`, generationID)
	if err != nil {
		return "", nil, fmt.Errorf("reading document event actor claims: %w", err)
	}
	defer func() { _ = rows.Close() }()

	claims := make([]DocumentEventActorClaim, 0)
	for rows.Next() {
		claim := DocumentEventActorClaim{GenerationID: generationID}
		var sensitive int
		if err := rows.Scan(&claim.EventID, &claim.Role, &claim.ActorKey, &claim.DisplayName,
			&claim.Address, &claim.Ordinal, &claim.EvidenceKind, &claim.EvidenceID,
			&claim.ClaimBasis, &claim.AxisKey, &claim.UTCKey, &sensitive); err != nil {
			return "", nil, fmt.Errorf("scanning document event actor claim: %w", err)
		}
		claim.Sensitive = sensitive != 0
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		return "", nil, fmt.Errorf("reading document event actor claims: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", nil, fmt.Errorf("closing document event actor snapshot: %w", err)
	}
	return generationID, claims, nil
}
