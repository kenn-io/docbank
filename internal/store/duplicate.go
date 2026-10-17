package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
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
	groups := []DuplicateGroup{group}
	if err := populateDuplicateReferences(ctx, tx, groups); err != nil {
		return DuplicateGroup{}, err
	}
	group = groups[0]
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

	// Group the live population once for both exact totals and page selection.
	// A totals row remains even when the requested page is empty.
	rows, err := tx.QueryContext(ctx, `WITH `+CurrentContentMembershipCTE+`,
		duplicate_groups AS (
			SELECT blob_hash, size, COUNT(DISTINCT node_id) AS reference_count
			FROM current_content_members
			GROUP BY blob_hash, size
			HAVING COUNT(DISTINCT node_id) >= 2
		), totals AS (
			SELECT COUNT(*) AS groups, COALESCE(SUM(reference_count), 0) AS refs
			FROM duplicate_groups
		), selected AS (
			SELECT * FROM duplicate_groups ORDER BY blob_hash ASC LIMIT ? OFFSET ?
		)
		SELECT totals.groups, totals.refs,
		       COALESCE(g.blob_hash, ''), COALESCE(g.size, 0), COALESCE(g.reference_count, 0),
		       COALESCE((SELECT node_id FROM current_content_members
		        WHERE blob_hash = g.blob_hash AND size = g.size
		        ORDER BY `+DuplicateRepresentativeOrder+` LIMIT 1), 0)
		FROM totals LEFT JOIN selected g ON true
		ORDER BY g.blob_hash ASC`, limit, offset)
	if err != nil {
		return DuplicatePage{}, fmt.Errorf("listing duplicate content: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var group DuplicateGroup
		group.References = make([]DuplicateReference, 0)
		if err := rows.Scan(
			&page.Total, &page.TotalReferences,
			&group.Hash, &group.Size, &group.ReferenceCount, &group.RepresentativeNodeID,
		); err != nil {
			return DuplicatePage{}, fmt.Errorf("scanning duplicate group: %w", err)
		}
		if group.Hash != "" {
			page.Items = append(page.Items, group)
		}
	}
	if err := rows.Err(); err != nil {
		return DuplicatePage{}, fmt.Errorf("listing duplicate content: %w", err)
	}
	if err := rows.Close(); err != nil {
		return DuplicatePage{}, fmt.Errorf("closing duplicate groups: %w", err)
	}

	if err := populateDuplicateReferences(ctx, tx, page.Items); err != nil {
		return DuplicatePage{}, err
	}
	if err := tx.Commit(); err != nil {
		return DuplicatePage{}, fmt.Errorf("closing duplicate snapshot: %w", err)
	}
	return page, nil
}

func populateDuplicateReferences(ctx context.Context, tx *sql.Tx, groups []DuplicateGroup) error {
	if len(groups) == 0 {
		return nil
	}
	identities := make([]struct {
		Hash string `json:"hash"`
		Size int64  `json:"size"`
	}, len(groups))
	for i, group := range groups {
		identities[i].Hash, identities[i].Size = group.Hash, group.Size
		groups[i].References = make([]DuplicateReference, 0, maxDuplicatePageReferences)
	}
	encoded, err := json.Marshal(identities)
	if err != nil {
		return fmt.Errorf("encoding duplicate groups: %w", err)
	}
	// Select at most sixteen references per group before loading their metadata.
	rows, err := tx.QueryContext(ctx, `WITH `+CurrentContentMembershipCTE+`
	SELECT `+nodeCols+`, cv.recorded_at, cv.node_revision,
	       cv.introduced_operation_id, cv.transition_kind, cv.source_version_id, g.key
	FROM json_each(?) AS g CROSS JOIN `+nodeFrom+`
	WHERE n.id IN (
		SELECT node_id FROM current_content_members
		WHERE blob_hash = json_extract(g.value, '$.hash') AND size = json_extract(g.value, '$.size')
		ORDER BY `+DuplicateRepresentativeOrder+` LIMIT ?
	)
	ORDER BY g.key, n.modified_at, n.id`, string(encoded), maxDuplicatePageReferences)
	if err != nil {
		return fmt.Errorf("listing duplicate references: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var index int
		ref := DuplicateReference{Reference: ContentReference{IsCurrent: true}}
		version := &ref.Reference.Version
		node, err := scanNode(rows, &version.RecordedAt, &version.NodeRevision,
			&version.IntroducedOperationID, &version.TransitionKind, &version.SourceVersionID, &index)
		if err != nil {
			return fmt.Errorf("scanning duplicate references: %w", err)
		}
		ref.Reference.Node = node
		version.ID, version.NodeID, version.BlobHash = node.CurrentVersionID, node.ID, node.BlobHash
		version.MD5, version.Size, version.MimeType = node.MD5, node.Size, node.MimeType
		groups[index].References = append(groups[index].References, ref)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("listing duplicate references: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing duplicate references: %w", err)
	}
	for i := range groups {
		groups[i].ReferencesTruncated = groups[i].ReferenceCount > len(groups[i].References)
	}
	return populateDuplicateReferenceDetails(ctx, tx, groups)
}

func populateDuplicateReferenceDetails(ctx context.Context, tx *sql.Tx, groups []DuplicateGroup) error {
	ids := make([]int64, 0, len(groups)*maxDuplicatePageReferences)
	references := make(map[int64]*DuplicateReference, cap(ids))
	for _, group := range groups {
		for i := range group.References {
			ref := &group.References[i]
			id := ref.Reference.Node.ID
			ids = append(ids, id)
			references[id] = ref
			ref.Collections = make([]DuplicateCollection, 0)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	paths, err := pathsOf(ctx, tx, ids)
	if err != nil {
		return err
	}
	for i, id := range ids {
		references[id].Reference.Path = paths[i]
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("encoding duplicate nodes: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `WITH `+CollectionMembershipCTE+`, ranked AS (
		SELECT cm.node_id, cm.ingest_id,
		       ROW_NUMBER() OVER (PARTITION BY cm.node_id ORDER BY cm.ingest_id) AS ordinal,
		       COUNT(*) OVER (PARTITION BY cm.node_id) AS total
		FROM collection_members cm
		WHERE cm.node_id IN (SELECT value FROM json_each(?))
	)
	SELECT ranked.node_id, ranked.ingest_id, ranked.total, l.label
	FROM ranked LEFT JOIN collection_labels l ON l.ingest_id=ranked.ingest_id
	WHERE ordinal<=? ORDER BY ranked.node_id, ranked.ingest_id`, string(encoded), maxDuplicateCollections)
	if err != nil {
		return fmt.Errorf("listing duplicate collections: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var collection DuplicateCollection
		var total int
		var label sql.NullString
		if err := rows.Scan(&id, &collection.ID, &total, &label); err != nil {
			return fmt.Errorf("scanning duplicate collections: %w", err)
		}
		ref := references[id]
		collection.Label = stringPtr(label)
		ref.Collections = append(ref.Collections, collection)
		ref.CollectionCount = total
		ref.CollectionsTruncated = total > maxDuplicateCollections
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("listing duplicate collections: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing duplicate collections: %w", err)
	}
	return nil
}
