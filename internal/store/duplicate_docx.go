package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/docbank/document"
)

const docxMediaType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

type DOCXDuplicateGroup struct {
	Fingerprint         string
	ReferenceCount      int
	References          []NodeView
	ReferencesTruncated bool
	hashes              []string
}

type DOCXDuplicatePage struct {
	Items                     []DOCXDuplicateGroup
	Total                     int
	TotalReferences           int
	UnfingerprintedReferences int
	Limit                     int
	Offset                    int
}

// DOCXDuplicates groups live current DOCX files using verified source metadata.
// It scans cached evidence, never originals, and leaves raw content identity intact.
func (s *Store) DOCXDuplicates(ctx context.Context, limit, offset int) (DOCXDuplicatePage, error) {
	if limit < 1 || limit > 100 || offset < 0 {
		return DOCXDuplicatePage{}, fmt.Errorf("%w: limit must be 1..100 and offset nonnegative", ErrInvalidDuplicatePage)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return DOCXDuplicatePage{}, fmt.Errorf("starting DOCX duplicate snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	page, err := docxDuplicateGroups(ctx, tx)
	if err != nil {
		return DOCXDuplicatePage{}, err
	}
	page.Limit, page.Offset = limit, offset
	start := min(offset, len(page.Items))
	page.Items = page.Items[start : start+min(limit, len(page.Items)-start)]
	for i := range page.Items {
		group := &page.Items[i]
		if err := s.populateDOCXReferences(ctx, tx, group); err != nil {
			return DOCXDuplicatePage{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return DOCXDuplicatePage{}, fmt.Errorf("closing DOCX duplicate snapshot: %w", err)
	}
	return page, nil
}

func docxDuplicateGroups(ctx context.Context, tx *sql.Tx) (DOCXDuplicatePage, error) {
	page := DOCXDuplicatePage{Items: []DOCXDuplicateGroup{}}
	rows, err := tx.QueryContext(ctx, `WITH `+CurrentContentMembershipCTE+`, candidates AS (
		SELECT cm.blob_hash, COUNT(*) AS refs FROM current_content_members cm
		JOIN content_versions cv ON cv.version_id=cm.version_id
		WHERE cv.mime_type=? GROUP BY cm.blob_hash
	)
	SELECT c.blob_hash, c.refs, g.canonical_json, g.checksum
	FROM candidates c LEFT JOIN source_metadata_heads h ON h.source_sha256=c.blob_hash
	LEFT JOIN source_metadata_generations g ON g.generation_id=h.generation_id`, docxMediaType)
	if err != nil {
		return page, fmt.Errorf("reading DOCX evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()
	groups := map[string]*DOCXDuplicateGroup{}
	for rows.Next() {
		var hash string
		var count int
		var encoded []byte
		var checksum sql.NullString
		if err := rows.Scan(&hash, &count, &encoded, &checksum); err != nil {
			return page, err
		}
		var fingerprint string
		if checksum.Valid {
			metadata, actual, err := document.DecodeSourceMetadataV1(encoded)
			if err != nil || actual != checksum.String {
				return page, ErrSourceMetadataCorrupt
			}
			for _, field := range metadata.Fields {
				if field.Key != document.DOCXPackageFingerprintKey {
					continue
				}
				if field.Value.Kind != document.SourceMetadataString || field.Value.String == nil {
					return page, ErrSourceMetadataCorrupt
				}
				fingerprint = *field.Value.String
				if err := validateCatalogSHA256(fingerprint, "DOCX fingerprint"); err != nil {
					return page, ErrSourceMetadataCorrupt
				}
			}
		}
		if fingerprint == "" {
			page.UnfingerprintedReferences += count
			continue
		}
		if groups[fingerprint] == nil {
			groups[fingerprint] = &DOCXDuplicateGroup{Fingerprint: fingerprint}
		}
		group := groups[fingerprint]
		group.ReferenceCount += count
		group.hashes = append(group.hashes, hash)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	for _, group := range groups {
		if group.ReferenceCount < 2 {
			continue
		}
		page.Items = append(page.Items, *group)
		page.TotalReferences += group.ReferenceCount
	}
	page.Total = len(page.Items)
	slices.SortFunc(page.Items, func(a, b DOCXDuplicateGroup) int { return strings.Compare(a.Fingerprint, b.Fingerprint) })
	return page, nil
}

func (s *Store) populateDOCXReferences(ctx context.Context, tx *sql.Tx, group *DOCXDuplicateGroup) error {
	encoded, err := json.Marshal(group.hashes)
	if err != nil {
		return fmt.Errorf("encoding DOCX group: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `WITH `+CurrentContentMembershipCTE+`
		SELECT `+nodeCols+` FROM `+nodeFrom+`
		WHERE n.id IN (SELECT node_id FROM current_content_members)
		AND cv.mime_type=? AND cv.blob_hash IN (SELECT value FROM json_each(?))
		ORDER BY n.modified_at, n.id LIMIT ?`, docxMediaType, string(encoded), maxDuplicatePageReferences)
	if err != nil {
		return fmt.Errorf("reading DOCX group members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	group.References = []NodeView{}
	var ids []int64
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return err
		}
		ids = append(ids, node.ID)
		group.References = append(group.References, NodeView{Node: node})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	paths, err := s.pathsOf(ctx, tx, ids)
	if err != nil {
		return err
	}
	for i := range group.References {
		group.References[i].Path = paths[i]
	}
	group.ReferencesTruncated = group.ReferenceCount > len(group.References)
	return nil
}
