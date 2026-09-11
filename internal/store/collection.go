package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CollectionMembershipCTE is the shared active-file membership relation used
// by every collection summary and member read. It intentionally omits WITH so
// callers can compose it with other CTEs.
const CollectionMembershipCTE = `collection_members AS (
    SELECT DISTINCT p.ingest_id, p.node_id
    FROM provenance p
    JOIN ingests i ON i.id = p.ingest_id
    JOIN nodes n ON n.id = p.node_id
    WHERE i.source_kind NOT LIKE 'embedded:%'
      AND n.kind = 'file' AND n.trashed_at IS NULL
      AND NOT EXISTS (SELECT 1 FROM provenance later WHERE later.supersedes = p.identity)
)`

// Collection is one document-bearing operational ingest and its current
// membership summary. LabelRevision advances independently of membership.
type Collection struct {
	ID, SourceKind, SourceDescription, StartedAt string
	FileCount, TotalBytes                        int64
	Label                                        *string
	LabelRevision                                int64
	LabelUpdatedAt                               string
	Coverage                                     ProcessingCoverage
}

// CollectionMemberPage binds one collection summary and member page to the
// same read snapshot.
type CollectionMemberPage struct {
	Collection Collection
	Items      []NodeView
	Total      int
}

const collectionColumns = `i.id, i.source_kind, i.source_desc, i.started_at,
	COUNT(cm.node_id), COALESCE(SUM(cv.size), 0), l.label,
	COALESCE(l.revision, 1), COALESCE(l.updated_at, i.started_at)`

func scanCollection(row interface{ Scan(args ...any) error }) (Collection, error) {
	var collection Collection
	var label sql.NullString
	err := row.Scan(&collection.ID, &collection.SourceKind,
		&collection.SourceDescription, &collection.StartedAt,
		&collection.FileCount, &collection.TotalBytes, &label,
		&collection.LabelRevision, &collection.LabelUpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Collection{}, ErrNotFound
	}
	if err != nil {
		return Collection{}, fmt.Errorf("scanning collection: %w", err)
	}
	collection.Label = stringPtr(label)
	return collection, nil
}

func collectionSummaryByID(
	ctx context.Context, q rowQuerier, id string,
) (Collection, error) {
	collection, err := scanCollection(q.QueryRowContext(ctx, `WITH `+CollectionMembershipCTE+`
		SELECT `+collectionColumns+`
		FROM ingests i
		LEFT JOIN collection_members cm ON cm.ingest_id=i.id
		LEFT JOIN nodes n ON n.id=cm.node_id
		LEFT JOIN content_versions cv ON cv.version_id=n.current_version_id
		LEFT JOIN collection_labels l ON l.ingest_id=i.id
		WHERE i.id=? AND i.source_kind NOT LIKE 'embedded:%'
		GROUP BY i.id, i.source_kind, i.source_desc, i.started_at,
			l.ingest_id, l.label, l.revision, l.updated_at
		HAVING COUNT(cm.node_id)>0 OR l.ingest_id IS NOT NULL`, id))
	if err != nil {
		return Collection{}, fmt.Errorf("collection %q: %w", id, err)
	}
	return collection, nil
}

// Collections lists current nonempty operational collections and the total
// number of such runs. The limit must be between 1 and 1000.
func (s *Store) Collections(
	ctx context.Context, limit, offset int, selections ...CoverageSelection,
) ([]Collection, int, error) {
	selection, err := normalizeCoverageSelection(selections)
	if err != nil {
		return nil, 0, err
	}
	if err := validatePage(limit, offset); err != nil {
		return nil, 0, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, fmt.Errorf("starting collection snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var total int
	if err := tx.QueryRowContext(ctx, `WITH `+CollectionMembershipCTE+`
		SELECT COUNT(DISTINCT ingest_id) FROM collection_members`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting collections: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `WITH `+CollectionMembershipCTE+`
		SELECT `+collectionColumns+`
		FROM ingests i
		JOIN collection_members cm ON cm.ingest_id=i.id
		JOIN nodes n ON n.id=cm.node_id
		JOIN content_versions cv ON cv.version_id=n.current_version_id
		LEFT JOIN collection_labels l ON l.ingest_id=i.id
		GROUP BY i.id, i.source_kind, i.source_desc, i.started_at,
			l.label, l.revision, l.updated_at
		ORDER BY i.started_at DESC, i.id
		LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("listing collections: %w", err)
	}
	defer func() { _ = rows.Close() }()
	collections := make([]Collection, 0)
	for rows.Next() {
		collection, scanErr := scanCollection(rows)
		if scanErr != nil {
			_ = rows.Close()
			return nil, 0, scanErr
		}
		collections = append(collections, collection)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, 0, fmt.Errorf("listing collections: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, fmt.Errorf("closing collections: %w", err)
	}
	generation, err := collectionGenerationTx(ctx, tx)
	if err != nil {
		return nil, 0, err
	}
	ids := make([]string, len(collections))
	for i := range collections {
		ids[i] = collections[i].ID
	}
	coverage, err := collectionCoverageTx(ctx, tx, ids, selection, generation)
	if err != nil {
		return nil, 0, err
	}
	for i := range collections {
		collections[i].Coverage = coverage[collections[i].ID]
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, fmt.Errorf("closing collection snapshot: %w", err)
	}
	return collections, total, nil
}

// CollectionByID returns one current operational collection. Retained label
// authority keeps direct access available when current membership is empty.
func (s *Store) CollectionByID(ctx context.Context, id string, selections ...CoverageSelection) (Collection, error) {
	selection, err := normalizeCoverageSelection(selections)
	if err != nil {
		return Collection{}, err
	}
	if err := validateUUIDv4(id); err != nil {
		return Collection{}, fmt.Errorf("collection %q: %w", id, ErrNotFound)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Collection{}, err
	}
	defer func() { _ = tx.Rollback() }()
	collection, err := collectionSummaryByID(ctx, tx, id)
	if err != nil {
		return Collection{}, err
	}
	generation, err := collectionGenerationTx(ctx, tx)
	if err != nil {
		return Collection{}, err
	}
	coverage, err := collectionCoverageTx(ctx, tx, []string{id}, selection, generation)
	if err != nil {
		return Collection{}, err
	}
	collection.Coverage = coverage[id]
	if err := tx.Commit(); err != nil {
		return Collection{}, err
	}
	return collection, nil
}

// CollectionMembers returns one current member page and collection summary
// from the same read transaction.
func (s *Store) CollectionMembers(
	ctx context.Context, id string, limit, offset int, selections ...CoverageSelection,
) (CollectionMemberPage, error) {
	selection, err := normalizeCoverageSelection(selections)
	if err != nil {
		return CollectionMemberPage{}, err
	}
	if err := validateUUIDv4(id); err != nil {
		return CollectionMemberPage{}, fmt.Errorf("collection %q: %w", id, ErrNotFound)
	}
	if err := validatePage(limit, offset); err != nil {
		return CollectionMemberPage{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return CollectionMemberPage{}, fmt.Errorf("starting collection member snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	collection, err := collectionSummaryByID(ctx, tx, id)
	if err != nil {
		return CollectionMemberPage{}, err
	}
	rows, err := tx.QueryContext(ctx, `WITH `+CollectionMembershipCTE+`
		SELECT `+nodeCols+`
		FROM `+nodeFrom+`
		JOIN collection_members cm ON cm.node_id=n.id
		WHERE cm.ingest_id=?
		ORDER BY n.name, n.id
		LIMIT ? OFFSET ?`, id, limit, offset)
	if err != nil {
		return CollectionMemberPage{}, fmt.Errorf("listing collection %q members: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	nodes := make([]Node, 0)
	for rows.Next() {
		node, scanErr := scanNode(rows)
		if scanErr != nil {
			_ = rows.Close()
			return CollectionMemberPage{}, scanErr
		}
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return CollectionMemberPage{}, fmt.Errorf("listing collection %q members: %w", id, err)
	}
	if err := rows.Close(); err != nil {
		return CollectionMemberPage{}, fmt.Errorf("closing collection %q members: %w", id, err)
	}
	items := make([]NodeView, 0, len(nodes))
	for _, node := range nodes {
		path, pathErr := pathOf(ctx, tx, node.ID)
		if pathErr != nil {
			return CollectionMemberPage{}, pathErr
		}
		items = append(items, NodeView{Node: node, Path: path})
	}
	generation, err := collectionGenerationTx(ctx, tx)
	if err != nil {
		return CollectionMemberPage{}, err
	}
	coverage, err := collectionCoverageTx(ctx, tx, []string{id}, selection, generation)
	if err != nil {
		return CollectionMemberPage{}, err
	}
	collection.Coverage = coverage[id]
	if err := tx.Commit(); err != nil {
		return CollectionMemberPage{}, fmt.Errorf("closing collection member snapshot: %w", err)
	}
	return CollectionMemberPage{
		Collection: collection, Items: items, Total: int(collection.FileCount),
	}, nil
}
