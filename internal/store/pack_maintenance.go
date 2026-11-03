package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
)

// Capture affected packs before removing mappings or logical membership.
// SQLite keeps its released triggers; PostgreSQL refreshes the same projections
// once per affected pack, inside the mutation's writer-locked transaction.
func (s *Store) packSummaryKeysTx(ctx context.Context, tx *sql.Tx, hashes []string) ([][2]string, error) {
	if s.postgresSchema == "" || len(hashes) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(hashes)
	if err != nil {
		return nil, err
	}
	d := metadataJSONDialect{postgres: true}
	var packs [][2]string
	err = queryRows(ctx, tx, `SELECT DISTINCT store_id,pack_id FROM blob_pack_entries
  WHERE blob_hash IN (SELECT value FROM `+d.arrayValues("$1")+`)`, []any{string(encoded)}, func(rows *sql.Rows) error {
		var key [2]string
		if err := rows.Scan(&key[0], &key[1]); err != nil {
			return err
		}
		packs = append(packs, key)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("finding affected pack summaries: %w", err)
	}
	return packs, nil
}

func (s *Store) refreshPackSummariesTx(ctx context.Context, tx *sql.Tx, packs [][2]string) error {
	if s.postgresSchema == "" {
		return nil
	}
	seen := make(map[[2]string]bool, len(packs))
	for _, key := range packs {
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, err := tx.ExecContext(ctx, `UPDATE blob_packs SET
   (live_entries,live_stored_bytes,live_raw_bytes,max_live_stored_len,max_live_raw_len) = (
    SELECT count(*),COALESCE(sum(e.stored_len),0),COALESCE(sum(e.raw_len),0),COALESCE(max(e.stored_len),0),COALESCE(max(e.raw_len),0)
    FROM blob_pack_entries e JOIN blobs b ON b.hash=e.blob_hash
    WHERE e.store_id=$1 AND e.pack_id=$2)
   WHERE store_id=$1 AND pack_id=$2`, key[0], key[1]); err != nil {
			return fmt.Errorf("refreshing pack %s in store %s: %w", key[1], key[0], err)
		}
	}
	return nil
}
