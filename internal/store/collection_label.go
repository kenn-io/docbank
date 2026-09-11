package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrInvalidCollectionLabel reports a label outside the canonical name policy.
var ErrInvalidCollectionLabel = errors.New("invalid collection label")

// CollectionLabel is the separately revisioned mutable name authority for one
// operational ingest collection.
type CollectionLabel struct {
	IngestID  string
	Label     *string
	Revision  int64
	UpdatedAt string
}

func normalizeOptionalCollectionLabel(label *string) (string, bool, error) {
	if label == nil {
		return "", false, nil
	}
	normalized, err := normalizeLabelName(*label, ErrInvalidCollectionLabel)
	if err != nil {
		return "", false, err
	}
	return normalized, true, nil
}

func collectionLabelTx(
	ctx context.Context, q rowQuerier, ingestID string,
) (CollectionLabel, bool, error) {
	var startedAt string
	var label, updatedAt, retained sql.NullString
	var revision sql.NullInt64
	err := q.QueryRowContext(ctx, `WITH `+CollectionMembershipCTE+`
		SELECT i.started_at, l.label, l.revision, l.updated_at, l.ingest_id
		FROM ingests i LEFT JOIN collection_labels l ON l.ingest_id=i.id
		WHERE i.id=? AND i.source_kind NOT LIKE 'embedded:%'
		  AND (l.ingest_id IS NOT NULL OR EXISTS(
			SELECT 1 FROM collection_members cm WHERE cm.ingest_id=i.id
		  ))`, ingestID).Scan(&startedAt, &label, &revision, &updatedAt, &retained)
	if errors.Is(err, sql.ErrNoRows) {
		return CollectionLabel{}, false, ErrNotFound
	}
	if err != nil {
		return CollectionLabel{}, false, fmt.Errorf("reading collection label %q: %w", ingestID, err)
	}
	result := CollectionLabel{
		IngestID: ingestID, Label: stringPtr(label), Revision: 1, UpdatedAt: startedAt,
	}
	if retained.Valid {
		result.Revision = revision.Int64
		result.UpdatedAt = updatedAt.String
	}
	return result, retained.Valid, nil
}

// CollectionLabel returns the current label fence. Eligible unlabeled runs
// expose a virtual null label at revision one and their immutable start time.
func (s *Store) CollectionLabel(ctx context.Context, id string) (CollectionLabel, error) {
	if err := validateUUIDv4(id); err != nil {
		return CollectionLabel{}, fmt.Errorf("collection %q: %w", id, ErrNotFound)
	}
	label, _, err := collectionLabelTx(ctx, s.db, id)
	return label, err
}

// SetCollectionLabel applies one revision-fenced label mutation. Clearing a
// retained label preserves its row and revision history.
func (s *Store) SetCollectionLabel(
	ctx context.Context, id string, revision int64, label *string,
) (CollectionLabel, error) {
	if err := validateUUIDv4(id); err != nil {
		return CollectionLabel{}, fmt.Errorf("collection %q: %w", id, ErrNotFound)
	}
	normalizedValue, hasLabel, err := normalizeOptionalCollectionLabel(label)
	if err != nil {
		return CollectionLabel{}, err
	}
	var normalized *string
	if hasLabel {
		normalized = &normalizedValue
	}
	var result CollectionLabel
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		current, retained, err := collectionLabelTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if revision != current.Revision {
			return fmt.Errorf("collection label %q at revision %d, expected %d: %w",
				id, current.Revision, revision, ErrStaleRevision)
		}
		if equalOptionalStrings(current.Label, normalized) {
			result = current
			return nil
		}
		updatedAt := monotonicCollectionLabelTime(current.UpdatedAt)
		if !retained {
			_, err = tx.ExecContext(ctx, `INSERT INTO collection_labels(
				ingest_id,label,revision,updated_at
			) VALUES(?,?,2,?)`, id, normalized, updatedAt)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE collection_labels
				SET label=?, revision=revision+1, updated_at=?
				WHERE ingest_id=?`, normalized, updatedAt, id)
		}
		if err != nil {
			if s.driver.IsUniqueViolation(err) {
				return fmt.Errorf("collection label %q: %w", optionalString(normalized), ErrExists)
			}
			return fmt.Errorf("updating collection label %q: %w", id, err)
		}
		result, _, err = collectionLabelTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return CollectionLabel{}, err
	}
	return result, nil
}

func equalOptionalStrings(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func monotonicCollectionLabelTime(prior string) string {
	now := nowRFC3339()
	if now < prior {
		return prior
	}
	return now
}

func insertInitialCollectionLabelTx(
	ctx context.Context, tx *sql.Tx, run IngestRun,
) error {
	if run.initialLabel == nil {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO collection_labels(
		ingest_id,label,revision,updated_at
	) VALUES(?,?,1,?)`, run.ID(), run.initialLabel, run.record.StartedAt)
	return err
}
