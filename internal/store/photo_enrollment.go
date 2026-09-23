package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// enrollPhotoCandidatesTx is called only after the caller has written all
// provenance relations for a newly created file. It never writes a human
// decision receipt and it is a no-op once audit authority is active.
func (s *Store) enrollPhotoCandidatesTx(ctx context.Context, tx *sql.Tx, nodeIDs ...int64) error {
	active, err := auditAuthorityActiveTx(ctx, tx)
	if err != nil {
		return err
	}
	if active {
		return nil
	}
	seen := make(map[int64]struct{}, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if _, ok := seen[nodeID]; ok {
			continue
		}
		seen[nodeID] = struct{}{}
		node, err := nodeByIDTx(tx, nodeID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if node.Kind != nodeKindFile || node.TrashedAt != nil {
			continue
		}
		facts := photoNodeFacts(node)
		if !facts.Qualifies || isGenericRawPhotoName(node.Name, node.MimeType) {
			continue
		}
		var existing string
		if err := tx.QueryRowContext(ctx, `SELECT asset_id FROM photo_files WHERE node_id=?`, nodeID).Scan(&existing); err == nil {
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("checking automatic photo enrollment for node %d: %w", nodeID, err)
		}
		if child, err := emailDocumentChildTx(ctx, tx, node.CurrentVersionID); err != nil {
			return err
		} else if child {
			continue
		}
		if _, err := photoAssetCreateTx(ctx, tx, nodeID, inferPhotoRole(facts), facts.AssetKind, nowRFC3339()); err != nil {
			return fmt.Errorf("enrolling photo node %d: %w", nodeID, err)
		}
	}
	return nil
}

func emailDocumentChildTx(ctx context.Context, tx *sql.Tx, versionID string) (bool, error) {
	if versionID == "" {
		return false, nil
	}
	var child bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM email_document_relations WHERE child_version_id=?)`, versionID).Scan(&child); err != nil {
		return false, fmt.Errorf("checking email child provenance: %w", err)
	}
	return child, nil
}

func isGenericRawPhotoName(name, mimeType string) bool {
	if strings.TrimSpace(mimeType) != "" && strings.ToLower(strings.TrimSpace(mimeType)) != "application/octet-stream" {
		return false
	}
	ext := strings.ToLower(name)
	if dot := strings.LastIndexByte(ext, '.'); dot >= 0 {
		ext = ext[dot:]
	}
	switch ext {
	case ".3fr", ".arw", ".cr2", ".cr3", ".dng", ".iiq", ".kdc", ".mef", ".mos", ".mrw", ".nef", ".nrw", ".orf", ".pef", ".raf", ".raw", ".rw2", ".rwl", ".sr2", ".srw", ".x3f":
		return true
	default:
		return false
	}
}
