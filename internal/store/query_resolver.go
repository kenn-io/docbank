package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
)

// queryResolver uses the caller's read view. Snapshot execution must pass its
// own transaction here rather than resolving against a second connection.
type queryResolver struct{ q rowQuerier }

func (r queryResolver) photoMetadataHeadsFirst(ctx context.Context, value string) (bool, error) {
	var dense bool
	err := r.q.QueryRowContext(ctx, `WITH capacity AS MATERIALIZED (SELECT COUNT(*)+1 AS n FROM source_metadata_heads)
 SELECT (SELECT COUNT(*) FROM (SELECT 1 FROM photo_metadata_fts WHERE photo_metadata_fts MATCH ? LIMIT (SELECT n FROM capacity))) = (SELECT n FROM capacity)`,
		`text : `+quoteFTSOperand(value, false)).Scan(&dense)
	if err != nil {
		return false, fmt.Errorf("checking photo metadata matches: %w", err)
	}
	return dense, nil
}

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
		case query.ReferencePerson:
			value = document.FoldPersonName(value)
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
	case query.ReferencePerson:
		ref.Dependency.Kind = kind
		safe, _ := ctx.Value(visiblePersonContextKey{}).(bool)
		if byID {
			sql := `SELECT person_id,revision FROM persons WHERE person_id=COALESCE((SELECT surviving_person_id FROM person_aliases WHERE retired_person_id=$1),$2) AND state<>'retired'`
			args := []any{value, value}
			if safe {
				sql += ` AND ` + personVisibleSQL(3)
				args = append(args, 1, boolInt(queryPeopleHideLocked(ctx)))
			}
			err = r.q.QueryRowContext(ctx, sql, args...).Scan(&ref.Dependency.ID, &ref.Dependency.Revision)
		} else {
			var count int
			sql := `SELECT COUNT(*),COALESCE(MIN(person_id),''),COALESCE(MIN(revision),0) FROM persons WHERE display_name_folded=$1 AND state<>'retired'`
			args := []any{value}
			if safe {
				sql += ` AND ` + personVisibleSQL(2)
				args = append(args, 1, boolInt(queryPeopleHideLocked(ctx)))
			}
			err = r.q.QueryRowContext(ctx, sql, args...).Scan(&count, &ref.Dependency.ID, &ref.Dependency.Revision)
			if err == nil && count == 0 {
				err = query.ErrUnknownReference
			}
			if err == nil && count > 1 {
				err = &query.ExpressionError{Message: "several people have this name; use a person ID"}
			}
		}
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
			label, _, err = collectionLabelTx(ctx, r.q, id, false)
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

// personMentionNames returns a bounded, stable list of literal search phrases.
func (r queryResolver) personMentionNames(ctx context.Context, id string) ([]string, error) {
	var name string
	if err := r.q.QueryRowContext(ctx, `SELECT display_name FROM persons WHERE person_id=?`, id).Scan(&name); err != nil {
		return nil, err
	}
	names := []string{name}
	seen := map[string]bool{document.FoldPersonName(name): true}
	q, ok := r.q.(rowsQuerier)
	if !ok {
		return nil, errors.New("person aliases require a rows querier")
	}
	visible, _ := ctx.Value(visiblePersonContextKey{}).(bool)
	rows, err := q.QueryContext(ctx, `SELECT i.value_display FROM person_identities i WHERE i.person_id=? AND i.kind='name_alias'
		AND (?=0 OR i.origin='operator' OR EXISTS(SELECT 1 FROM document_people dp
			WHERE dp.person_id=i.person_id AND dp.actor_key=i.kind || ':' || i.value_normalized
			AND `+personVisibleActorSQL("dp", "?")+`)) ORDER BY i.value_normalized,i.identity_id`, id, boolInt(visible), boolInt(queryPeopleHideLocked(ctx)))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for len(names) < 16 && rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return nil, err
		}
		folded := document.FoldPersonName(alias)
		if folded != "" && !seen[folded] {
			names = append(names, alias)
			seen[folded] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return names, nil
}
