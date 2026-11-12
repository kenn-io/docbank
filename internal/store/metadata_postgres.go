package store

import (
	"context"
	"database/sql"
	"fmt"
	"io"

	"go.kenn.io/docbank/internal/store/pgschema"
)

// ImportPostgresMetadata restores logical metadata through setup credentials.
// The caller owns the pool. Blob bytes and physical pack authority must be
// restored and verified separately before the destination can be published.
func ImportPostgresMetadata(ctx context.Context, db *sql.DB, cfg pgschema.Config, input io.Reader) error {
	if _, err := pgschema.Setup(ctx, db, cfg); err != nil {
		return err
	}
	s := &Store{db: db, writeDB: db, postgresSchema: cfg.Schema}
	return s.ImportMetadata(ctx, input)
}

func insertPostgresHiddenFailure(ctx context.Context, tx *sql.Tx, v metadataHiddenFailure) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO photo_hidden_failures(failure_id,occurred_at)
		OVERRIDING SYSTEM VALUE VALUES($1,$2)`, v.ID, v.OccurredAt)
	return err
}

func insertPostgresIngest(ctx context.Context, tx *sql.Tx, v metadataIngest) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO ingests(id,started_at,source_kind,source_desc)
		VALUES($1,$2,$3,$4)`, v.ID, v.StartedAt, []byte(v.SourceKind), []byte(v.SourceDesc))
	return err
}

// SQLite import triggers own these projections. Rebuild after all records exist.
func (s *Store) refreshImportedCatalogsTx(ctx context.Context, tx *sql.Tx) error {
	for after := ""; ; {
		type member struct {
			id, asset string
			node      int64
		}
		var members []member
		if err := queryRows(ctx, tx, `SELECT file_id,node_id,COALESCE(asset_id,'') FROM photo_files
            WHERE file_id>$1 ORDER BY file_id LIMIT 100`, []any{after}, func(rows *sql.Rows) error {
			var item member
			if err := rows.Scan(&item.id, &item.node, &item.asset); err != nil {
				return err
			}
			members = append(members, item)
			return nil
		}); err != nil {
			return fmt.Errorf("reading imported photo projections: %w", err)
		}
		if len(members) == 0 {
			break
		}
		assets := map[string]bool{}
		for _, item := range members {
			if err := s.refreshPhotoTimelineTx(ctx, tx, item.node); err != nil {
				return err
			}
			if item.asset != "" {
				assets[item.asset] = true
			}
			after = item.id
		}
		for asset := range assets {
			if err := s.refreshPhotoAssetWorkTx(ctx, tx, asset); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO person_candidate_faces(candidate_id,face_id,content_version_id)
        SELECT c.candidate_id,e.value->>'evidence_id',e.value->>'content_version_id'
        FROM person_match_candidates c
        CROSS JOIN LATERAL jsonb_array_elements(convert_from(c.evidence_json,'UTF8')::jsonb) e(value)
        WHERE c.reason=$1 AND e.value->>'evidence_kind'='face'`, personCandidateReasonFaceMatch); err != nil {
		return fmt.Errorf("restoring candidate face membership: %w", err)
	}
	return nil
}

func insertPostgresProvenance(ctx context.Context, tx *sql.Tx, v metadataProvenance) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO provenance(identity,node_id,ingest_id,original_path,original_mtime,supersedes)
		VALUES($1,$2,$3,$4,$5,$6)`, v.Identity, v.NodeID, v.IngestID, []byte(v.OriginalPath), v.OriginalMTime, v.Supersedes)
	return err
}
