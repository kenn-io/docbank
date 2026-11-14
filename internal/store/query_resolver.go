package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

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
		if byID {
			err = r.q.QueryRowContext(ctx, `SELECT person_id,revision FROM persons WHERE person_id=COALESCE((SELECT surviving_person_id FROM person_aliases WHERE retired_person_id=?),?) AND state<>'retired'`, value, value).Scan(&ref.Dependency.ID, &ref.Dependency.Revision)
		} else {
			var count int
			err = r.q.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MIN(person_id),''),COALESCE(MIN(revision),0) FROM persons WHERE display_name_folded=? AND state<>'retired'`, value).Scan(&count, &ref.Dependency.ID, &ref.Dependency.Revision)
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

// personMentionNames includes aliases and names absorbed by the current person.
func (r queryResolver) personMentionNames(ctx context.Context, id string) ([]string, error) {
	var name string
	if err := r.q.QueryRowContext(ctx, `SELECT display_name FROM persons WHERE person_id=?`, id).Scan(&name); err != nil {
		return nil, err
	}
	var names []string
	if !strings.ContainsRune(name, '\x00') {
		names = append(names, name)
	}
	q, ok := r.q.(rowsQuerier)
	if !ok {
		return nil, errors.New("person aliases require a rows querier")
	}
	rows, err := q.QueryContext(ctx, `SELECT i.value_display FROM person_identities i WHERE i.person_id=? AND i.kind='name_alias' UNION SELECT m.absorbed_display_name FROM person_merges m JOIN person_aliases a ON a.retired_person_id=m.absorbed_person_id WHERE a.surviving_person_id=? ORDER BY 1`, id, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return nil, err
		}
		if alias != "" && alias != name && !strings.ContainsRune(alias, '\x00') {
			names = append(names, alias)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return names, nil
}
