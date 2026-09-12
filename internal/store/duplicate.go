package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const (
	maxDuplicatePageReferences = 16
	maxDuplicateCollections    = 16
)

// CurrentContentMembershipCTE is the shared live-current content relation for
// duplicate discovery and later matched-content features. It intentionally
// omits WITH so callers can compose it with other CTEs.
const CurrentContentMembershipCTE = `current_content_members AS (
    SELECT n.id AS node_id, v.version_id, v.blob_hash, b.size, n.modified_at
    FROM nodes n
    JOIN content_versions v
      ON v.node_id = n.id AND v.version_id = n.current_version_id
    JOIN blobs b ON b.hash = v.blob_hash AND b.size = v.size
    WHERE n.kind = 'file' AND n.trashed_at IS NULL
)`

// DuplicateRepresentativeOrder is the exact stable order for choosing a
// representative from a matched population. Future matched-content callers
// must apply it after establishing their population, not to a global
// prefiltered winner.
const DuplicateRepresentativeOrder = `modified_at ASC, node_id ASC`

// ErrInvalidDuplicatePage reports a duplicate page outside its bounded range.
var ErrInvalidDuplicatePage = errors.New("invalid duplicate page")

// DuplicateCollection is one current operational collection containing a
// duplicate reference.
type DuplicateCollection struct {
	ID    string
	Label *string
}

// DuplicateReference binds one live current content reference to its current
// operational collection memberships.
type DuplicateReference struct {
	Reference            ContentReference
	Collections          []DuplicateCollection
	CollectionCount      int
	CollectionsTruncated bool
}

// DuplicateGroup describes all live current file nodes sharing one canonical
// blob hash.
type DuplicateGroup struct {
	Hash                 string
	Size                 int64
	ReferenceCount       int
	RepresentativeNodeID int64
	References           []DuplicateReference
	ReferencesTruncated  bool
}

// DuplicatePage is one hash-ordered page of duplicate groups. Totals describe
// all matching groups and references independently of the selected page.
type DuplicatePage struct {
	Items           []DuplicateGroup
	Total           int
	TotalReferences int
	Limit           int
	Offset          int
}

// DuplicateGroupByHash returns the bounded live-current group for one exact
// selected content identity. It uses the same eligible relation as Duplicates
// and does not scan or reinterpret the paginated group listing.
func (s *Store) DuplicateGroupByHash(ctx context.Context, hash string, size int64) (DuplicateGroup, error) {
	if validateCatalogSHA256(hash, "duplicate hash") != nil || size < 0 {
		return DuplicateGroup{}, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DuplicateGroup{}, fmt.Errorf("starting exact duplicate snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	group := DuplicateGroup{Hash: hash, Size: size, References: make([]DuplicateReference, 0)}
	err = tx.QueryRowContext(ctx, `WITH `+CurrentContentMembershipCTE+`
		SELECT COUNT(DISTINCT node_id),
		       (SELECT node_id FROM current_content_members
		        WHERE blob_hash=? AND size=? ORDER BY `+DuplicateRepresentativeOrder+` LIMIT 1)
		FROM current_content_members WHERE blob_hash=? AND size=?
		HAVING COUNT(DISTINCT node_id) >= 2`, hash, size, hash, size,
	).Scan(&group.ReferenceCount, &group.RepresentativeNodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return DuplicateGroup{}, ErrNotFound
	}
	if err != nil {
		return DuplicateGroup{}, fmt.Errorf("reading exact duplicate group %s: %w", hash, err)
	}
	group.References, err = duplicateReferences(ctx, tx, group)
	if err != nil {
		return DuplicateGroup{}, err
	}
	group.ReferencesTruncated = group.ReferenceCount > len(group.References)
	if err := tx.Commit(); err != nil {
		return DuplicateGroup{}, fmt.Errorf("closing exact duplicate snapshot: %w", err)
	}
	return group, nil
}

// Duplicates lists groups of at least two distinct live files whose exact
// current versions share canonical blob content.
func (s *Store) Duplicates(
	ctx context.Context, limit, offset int,
) (DuplicatePage, error) {
	if limit < 1 || limit > 100 {
		return DuplicatePage{}, fmt.Errorf(
			"%w: limit must be between 1 and 100", ErrInvalidDuplicatePage,
		)
	}
	if offset < 0 {
		return DuplicatePage{}, fmt.Errorf(
			"%w: offset must not be negative", ErrInvalidDuplicatePage,
		)
	}

	page := DuplicatePage{
		Items: make([]DuplicateGroup, 0), Limit: limit, Offset: offset,
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DuplicatePage{}, fmt.Errorf("starting duplicate snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	groupsSQL := `WITH ` + CurrentContentMembershipCTE + `,
		duplicate_groups AS (
			SELECT blob_hash, size, COUNT(DISTINCT node_id) AS reference_count
			FROM current_content_members
			GROUP BY blob_hash, size
			HAVING COUNT(DISTINCT node_id) >= 2
		)`
	if err := tx.QueryRowContext(ctx, groupsSQL+`
		SELECT COUNT(*), COALESCE(SUM(reference_count), 0)
		FROM duplicate_groups`).Scan(&page.Total, &page.TotalReferences); err != nil {
		return DuplicatePage{}, fmt.Errorf("counting duplicate content: %w", err)
	}

	rows, err := tx.QueryContext(ctx, groupsSQL+`
		SELECT g.blob_hash, g.size, g.reference_count,
		       (SELECT node_id FROM current_content_members
		        WHERE blob_hash = g.blob_hash AND size = g.size
		        ORDER BY `+DuplicateRepresentativeOrder+` LIMIT 1)
		FROM duplicate_groups g
		ORDER BY g.blob_hash ASC
		LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return DuplicatePage{}, fmt.Errorf("listing duplicate content: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var group DuplicateGroup
		group.References = make([]DuplicateReference, 0)
		if err := rows.Scan(
			&group.Hash, &group.Size, &group.ReferenceCount, &group.RepresentativeNodeID,
		); err != nil {
			return DuplicatePage{}, fmt.Errorf("scanning duplicate group: %w", err)
		}
		page.Items = append(page.Items, group)
	}
	if err := rows.Err(); err != nil {
		return DuplicatePage{}, fmt.Errorf("listing duplicate content: %w", err)
	}
	if err := rows.Close(); err != nil {
		return DuplicatePage{}, fmt.Errorf("closing duplicate groups: %w", err)
	}

	for index := range page.Items {
		references, err := duplicateReferences(ctx, tx, page.Items[index])
		if err != nil {
			return DuplicatePage{}, err
		}
		page.Items[index].References = references
		page.Items[index].ReferencesTruncated =
			page.Items[index].ReferenceCount > len(references)
	}
	if err := tx.Commit(); err != nil {
		return DuplicatePage{}, fmt.Errorf("closing duplicate snapshot: %w", err)
	}
	return page, nil
}

type duplicateReferenceIdentity struct {
	nodeID    int64
	versionID string
}

func duplicateReferences(
	ctx context.Context, tx *sql.Tx, group DuplicateGroup,
) ([]DuplicateReference, error) {
	rows, err := tx.QueryContext(ctx, `WITH `+CurrentContentMembershipCTE+`
		SELECT node_id, version_id
		FROM current_content_members
		WHERE blob_hash = ? AND size = ?
		ORDER BY `+DuplicateRepresentativeOrder+`
		LIMIT ?`, group.Hash, group.Size, maxDuplicatePageReferences)
	if err != nil {
		return nil, fmt.Errorf("listing references for duplicate %s: %w", group.Hash, err)
	}
	defer func() { _ = rows.Close() }()
	identities := make([]duplicateReferenceIdentity, 0, maxDuplicatePageReferences)
	for rows.Next() {
		var identity duplicateReferenceIdentity
		if err := rows.Scan(&identity.nodeID, &identity.versionID); err != nil {
			return nil, fmt.Errorf("scanning references for duplicate %s: %w", group.Hash, err)
		}
		identities = append(identities, identity)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing references for duplicate %s: %w", group.Hash, err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("closing references for duplicate %s: %w", group.Hash, err)
	}

	references := make([]DuplicateReference, 0, len(identities))
	for _, identity := range identities {
		node, err := nodeByIDTx(tx, identity.nodeID)
		if err != nil {
			return nil, fmt.Errorf("reading duplicate node %d: %w", identity.nodeID, err)
		}
		version, err := scanContentVersion(tx.QueryRowContext(ctx,
			`SELECT `+contentVersionCols+`
			 FROM content_versions WHERE node_id=? AND version_id=?`,
			identity.nodeID, identity.versionID))
		if err != nil {
			return nil, fmt.Errorf("reading duplicate node %d version: %w", identity.nodeID, err)
		}
		path, err := pathOf(ctx, tx, identity.nodeID)
		if err != nil {
			return nil, err
		}
		collections, collectionCount, err := duplicateCollections(ctx, tx, identity.nodeID)
		if err != nil {
			return nil, err
		}
		references = append(references, DuplicateReference{
			Reference: ContentReference{
				Version: version, Node: node, Path: path, IsCurrent: true,
			},
			Collections: collections, CollectionCount: collectionCount,
			CollectionsTruncated: collectionCount > len(collections),
		})
	}
	return references, nil
}

func duplicateCollections(
	ctx context.Context, tx *sql.Tx, nodeID int64,
) ([]DuplicateCollection, int, error) {
	var total int
	if err := tx.QueryRowContext(ctx, `WITH `+CollectionMembershipCTE+`
		SELECT COUNT(*) FROM collection_members WHERE node_id=?`, nodeID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting collections for duplicate node %d: %w", nodeID, err)
	}
	rows, err := tx.QueryContext(ctx, `WITH `+CollectionMembershipCTE+`
		SELECT cm.ingest_id, l.label
		FROM collection_members cm
		LEFT JOIN collection_labels l ON l.ingest_id=cm.ingest_id
		WHERE cm.node_id=?
		ORDER BY cm.ingest_id ASC
		LIMIT ?`, nodeID, maxDuplicateCollections)
	if err != nil {
		return nil, 0, fmt.Errorf("listing collections for duplicate node %d: %w", nodeID, err)
	}
	defer func() { _ = rows.Close() }()
	collections := make([]DuplicateCollection, 0, min(total, maxDuplicateCollections))
	for rows.Next() {
		var collection DuplicateCollection
		var label sql.NullString
		if err := rows.Scan(&collection.ID, &label); err != nil {
			return nil, 0, fmt.Errorf(
				"scanning collections for duplicate node %d: %w", nodeID, err,
			)
		}
		collection.Label = stringPtr(label)
		collections = append(collections, collection)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("listing collections for duplicate node %d: %w", nodeID, err)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, fmt.Errorf("closing collections for duplicate node %d: %w", nodeID, err)
	}
	return collections, total, nil
}
