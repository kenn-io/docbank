package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const MaxProvenancePageSize = 1000

// ProvenanceFact is one immutable statement about where a document entered
// Docbank. A newer fact may supersede an older fact without rewriting it.
type ProvenanceFact struct {
	Identity          string
	NodeID            int64
	IngestID          string
	IngestStartedAt   string
	SourceKind        string
	SourceDescription string
	OriginalPath      string
	OriginalMTime     *string
	Supersedes        *string
	Active            bool
}

// NodeProvenancePage is one transactionally consistent document and bounded
// page of its newest-ingest-first provenance facts. Path is empty for trash.
type NodeProvenancePage struct {
	Node   Node
	Path   string
	Items  []ProvenanceFact
	Total  int
	Limit  int
	Offset int
}

// NodeProvenance returns immutable origin facts for one file node. The node,
// live path, count, and page come from one read snapshot so the response cannot
// combine provenance with a later move or trash operation.
func (s *Store) NodeProvenance(
	ctx context.Context, nodeID int64, limit, offset int,
) (NodeProvenancePage, error) {
	if limit < 1 || limit > MaxProvenancePageSize {
		return NodeProvenancePage{}, fmt.Errorf(
			"provenance limit must be between 1 and %d", MaxProvenancePageSize,
		)
	}
	if offset < 0 {
		return NodeProvenancePage{}, errors.New("provenance offset must not be negative")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return NodeProvenancePage{}, fmt.Errorf("starting provenance snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	page := NodeProvenancePage{Items: []ProvenanceFact{}, Limit: limit, Offset: offset}
	page.Node, err = nodeByIDTx(tx, nodeID)
	if err != nil {
		return NodeProvenancePage{}, err
	}
	if page.Node.IsDir() {
		return NodeProvenancePage{}, fmt.Errorf("node %d: %w", nodeID, ErrNotFile)
	}
	if page.Node.TrashedAt == nil {
		page.Path, err = pathOf(ctx, tx, nodeID)
		if err != nil {
			return NodeProvenancePage{}, err
		}
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM provenance WHERE node_id = ?`, nodeID,
	).Scan(&page.Total); err != nil {
		return NodeProvenancePage{}, fmt.Errorf("counting provenance for node %d: %w", nodeID, err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT p.identity, p.node_id, p.ingest_id, i.started_at,
		       i.source_kind, i.source_desc, p.original_path,
		       p.original_mtime, p.supersedes,
		       NOT EXISTS(SELECT 1 FROM provenance next WHERE next.supersedes = p.identity)
		FROM provenance p JOIN ingests i ON i.id = p.ingest_id
		WHERE p.node_id = ?
		ORDER BY i.started_at DESC, p.identity DESC
		LIMIT ? OFFSET ?`, nodeID, limit, offset)
	if err != nil {
		return NodeProvenancePage{}, fmt.Errorf("listing provenance for node %d: %w", nodeID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var fact ProvenanceFact
		var originalMTime, supersedes sql.NullString
		if err := rows.Scan(
			&fact.Identity, &fact.NodeID, &fact.IngestID, &fact.IngestStartedAt,
			&fact.SourceKind, &fact.SourceDescription, &fact.OriginalPath,
			&originalMTime, &supersedes, &fact.Active,
		); err != nil {
			return NodeProvenancePage{}, fmt.Errorf("scanning provenance for node %d: %w", nodeID, err)
		}
		fact.OriginalMTime = stringPtr(originalMTime)
		fact.Supersedes = stringPtr(supersedes)
		page.Items = append(page.Items, fact)
	}
	if err := rows.Err(); err != nil {
		return NodeProvenancePage{}, fmt.Errorf("listing provenance for node %d: %w", nodeID, err)
	}
	if err := tx.Commit(); err != nil {
		return NodeProvenancePage{}, fmt.Errorf("closing provenance snapshot: %w", err)
	}
	return page, nil
}
