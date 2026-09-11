package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/query"
)

// queryResolver uses the caller's read view. Snapshot execution must pass its
// own transaction here rather than resolving against a second connection.
type queryResolver struct{ q rowQuerier }

func (r queryResolver) Resolve(
	ctx context.Context, kind query.ReferenceKind, value string, byID bool,
) (query.Reference, error) {
	if err := ctx.Err(); err != nil {
		return query.Reference{}, err
	}
	if byID {
		if err := validateUUIDv4(value); err != nil {
			return query.Reference{}, query.ErrUnknownReference
		}
	} else {
		var err error
		switch kind {
		case query.ReferenceTag:
			value, err = NormalizeTagName(value)
		case query.ReferenceCollection:
			value, _, err = normalizeOptionalCollectionLabel(&value)
		case query.ReferenceSaved:
			value, err = normalizeLabelName(value, ErrInvalidSavedQuery)
		default:
			return query.Reference{}, fmt.Errorf("unsupported reference kind %q", kind)
		}
		if err != nil {
			return query.Reference{}, query.ErrUnknownReference
		}
	}
	var ref query.Reference
	var err error
	switch kind {
	case query.ReferenceTag:
		selector := "name=?"
		if byID {
			selector = "id=?"
		}
		ref.Dependency.Kind = kind
		err = r.q.QueryRowContext(ctx, `SELECT id, revision FROM tags WHERE `+selector, value).
			Scan(&ref.Dependency.ID, &ref.Dependency.Revision)
	case query.ReferenceCollection:
		id := value
		if !byID {
			err = r.q.QueryRowContext(ctx, `SELECT ingest_id FROM collection_labels WHERE label=?`, value).Scan(&id)
		}
		if err == nil {
			var label CollectionLabel
			label, _, err = collectionLabelTx(ctx, r.q, id)
			ref.Dependency = query.Dependency{Kind: kind, ID: id, Revision: label.Revision}
		}
	case query.ReferenceSaved:
		ref, err = r.saved(ctx, value, byID)
	default:
		return query.Reference{}, fmt.Errorf("unsupported reference kind %q", kind)
	}
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
		return query.Reference{}, query.ErrUnknownReference
	}
	if err != nil {
		return query.Reference{}, fmt.Errorf("resolving %s reference: %w", kind, err)
	}
	return ref, nil
}

func (r queryResolver) saved(ctx context.Context, value string, byID bool) (query.Reference, error) {
	selector := "name=?"
	if byID {
		selector = "id=?"
	}
	record, err := scanSavedQuery(r.q.QueryRowContext(ctx, `
		SELECT id,name,description,kind,payload,fingerprint,revision,created_at,updated_at
		FROM saved_queries WHERE `+selector+` AND kind=?`, value, SavedQueryKindQuery))
	if err != nil {
		return query.Reference{}, err
	}
	canonical, fingerprint, err := canonicalizeSavedQueryPayload(record.Kind, record.Payload)
	if err != nil {
		return query.Reference{}, fmt.Errorf("invalid stored query %s: %w", record.ID, err)
	}
	if fingerprint != record.Fingerprint {
		return query.Reference{}, fmt.Errorf("stored query %s fingerprint mismatch", record.ID)
	}
	parsed, err := query.Parse(canonical)
	if err != nil {
		return query.Reference{}, fmt.Errorf("decoding stored query %s: %w", record.ID, err)
	}
	return query.Reference{
		Dependency: query.Dependency{Kind: query.ReferenceSaved, ID: record.ID, Revision: record.Revision},
		Query:      &parsed,
	}, nil
}

var _ query.Resolver = queryResolver{}
