package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// RebindWatchedSource changes the operational cursor without changing archived content or placement.
func (s *Store) RebindWatchedSource(ctx context.Context, watchName, oldRef, newRef string, expectedNodeID int64) (Node, error) {
	for _, ref := range []string{oldRef, newRef} {
		if err := validateWatchSourceRecord(metadataWatchSource{
			Type: metadataWatchSourceType, WatchName: watchName, SourceRef: ref,
			NodeID: expectedNodeID, BlobHash: strings.Repeat("0", 64), Size: 0,
		}); err != nil {
			return Node{}, err
		}
	}
	if oldRef == newRef {
		return Node{}, errors.New("watched source destination is unchanged")
	}
	var node Node
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var owned, occupied bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM push_sources WHERE node_id=?)`, expectedNodeID).Scan(&owned); err != nil {
			return err
		}
		if owned {
			return fmt.Errorf("watched node %d is already owned by push", expectedNodeID)
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM push_sources WHERE push_name=? AND source_ref=?)`, watchName, newRef).Scan(&occupied); err != nil {
			return err
		}
		if occupied {
			return fmt.Errorf("watched destination %q/%q is owned by push", watchName, newRef)
		}
		old, err := watchSourceTx(ctx, tx, watchName, oldRef)
		retry := errors.Is(err, ErrNotFound)
		if err != nil && !retry {
			return err
		}
		if !retry && old.nodeID != expectedNodeID {
			return fmt.Errorf("watched source %q/%q has another node", watchName, oldRef)
		}
		destination, err := watchSourceTx(ctx, tx, watchName, newRef)
		switch {
		case err == nil && (!retry || destination.nodeID != expectedNodeID):
			return fmt.Errorf("watched destination %q/%q is occupied", watchName, newRef)
		case err != nil && !errors.Is(err, ErrNotFound):
			return err
		case retry && errors.Is(err, ErrNotFound):
			return fmt.Errorf("watched source %q/%q: %w", watchName, oldRef, ErrNotFound)
		}
		node, err = nodeByIDTx(tx, expectedNodeID)
		if err != nil {
			return err
		}
		if node.TrashedAt != nil {
			return fmt.Errorf("watched node %d is trashed", expectedNodeID)
		}
		if node.IsDir() {
			return fmt.Errorf("watched node %d: %w", node.ID, ErrNotFile)
		}
		if retry {
			return nil
		}
		result, err := tx.ExecContext(ctx, `UPDATE watch_sources SET source_ref=? WHERE watch_name=? AND source_ref=? AND node_id=?`, newRef, watchName, oldRef, expectedNodeID)
		if err != nil {
			return fmt.Errorf("rebinding watched source: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("rebinding watched source: %w", ErrNotFound)
		}
		return nil
	})
	if err != nil {
		return Node{}, err
	}
	return node, nil
}
