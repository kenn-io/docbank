package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
)

// DerivativePurgeFingerprint seals the selected immutable derivative identities
// and their current heads in one read snapshot. Unrelated source metadata and
// global lexical-generation replacements do not change this selection. Roots
// and physical reachability are still checked by PurgeDerivatives at execution.
func (s *Store) DerivativePurgeFingerprint(ctx context.Context, request PurgeRequest) (_ string, retErr error) {
	args := make([]any, 0, 4)
	for _, ids := range [][]string{request.ContentVersionIDs, request.AttachmentIDs, request.BuildIDs} {
		encoded, err := json.Marshal(ids)
		if err != nil {
			return "", err
		}
		args = append(args, string(encoded))
	}
	args = append(args, request.All)
	d := metadataJSONDialect{postgres: s.postgresSchema != ""}
	array := "json_array"
	collation := ""
	if d.postgres {
		array = "json_build_array"
		collation = `COLLATE "C"`
	}
	rows, err := s.db.QueryContext(ctx, `WITH
 versions AS (SELECT value AS id FROM `+d.arrayValues("$1")+`),
 attachments AS (SELECT value AS id FROM `+d.arrayValues("$2")+`),
 builds AS (SELECT value AS id FROM `+d.arrayValues("$3")+`),
 selected_attachments AS (
 SELECT a.* FROM rendition_attachments a
 WHERE $4 OR a.content_version_id IN (SELECT id FROM versions) OR a.attachment_id IN (SELECT id FROM attachments) OR a.build_id IN (SELECT id FROM builds)
 ),
 selected_builds AS (
 SELECT build_id FROM rendition_builds
 WHERE $4 OR build_id IN (SELECT id FROM builds) OR build_id IN (SELECT build_id FROM selected_attachments)
 OR (provider_operation_id='`+legacyPlainTextProvider+`' AND source_sha256 IN
 (SELECT blob_hash FROM content_versions WHERE version_id IN (SELECT id FROM versions)))
 ),
 selected_generations AS (
 SELECT g.generation_id FROM embedding_input_generations g
 LEFT JOIN rendition_attachments a ON a.attachment_id=g.attachment_id
 WHERE $4 OR g.source_version_id IN (SELECT id FROM versions) OR g.attachment_id IN (SELECT id FROM attachments) OR a.build_id IN (SELECT id FROM builds)
 ),
 selected_sets AS (
 SELECT s.* FROM embedding_sets s
 JOIN embedding_input_generations g ON g.generation_id=s.input_generation_id
 LEFT JOIN rendition_attachments a ON a.attachment_id=g.attachment_id
 WHERE $4 OR s.content_version_id IN (SELECT id FROM versions) OR g.attachment_id IN (SELECT id FROM attachments) OR a.build_id IN (SELECT id FROM builds)
 ),
 selected_heads AS (
 SELECT h.* FROM rendition_heads h JOIN selected_attachments a ON a.attachment_id=h.attachment_id
 )
 SELECT CAST(`+array+`('attachment',a.attachment_id,a.content_version_id,a.build_id,a.profile_fingerprint) AS TEXT) `+collation+` AS identity
 FROM selected_attachments a
 UNION ALL SELECT CAST(`+array+`('build',build_id) AS TEXT) FROM selected_builds
 UNION ALL SELECT CAST(`+array+`('head',content_version_id,profile_fingerprint,attachment_id) AS TEXT) FROM selected_heads
 UNION ALL SELECT CAST(`+array+`('embedding_generation',generation_id) AS TEXT) FROM selected_generations
 UNION ALL SELECT CAST(`+array+`('embedding_vector',vector_set_id) AS TEXT) FROM embedding_vector_sets WHERE $4
 UNION ALL SELECT CAST(`+array+`('embedding_set',embedding_set_id) AS TEXT) FROM selected_sets
 UNION ALL SELECT CAST(`+array+`('embedding_head',h.content_version_id,h.binding_id,h.input_kind,h.embedding_set_id) AS TEXT)
 FROM embedding_heads h JOIN selected_sets s ON s.embedding_set_id=h.embedding_set_id
 UNION ALL SELECT CAST(`+array+`('rendition_waiter',j.job_id,w.waiter_id,w.content_version_id,w.profile_fingerprint,w.attachment_id) AS TEXT)
 FROM rendition_jobs j LEFT JOIN rendition_job_waiters w ON w.job_id=j.job_id
 WHERE $4 OR j.job_id IN (SELECT id FROM builds) OR w.content_version_id IN (SELECT id FROM versions) OR w.attachment_id IN (SELECT id FROM attachments)
 UNION ALL SELECT CAST(`+array+`('embedding_job',j.job_id) AS TEXT)
 FROM embedding_jobs j
 WHERE $4 OR j.content_version_id IN (SELECT id FROM versions) OR j.generation_id IN (SELECT generation_id FROM selected_generations)
 OR (j.input_kind='rendition_chunk' AND EXISTS (
 SELECT 1 FROM selected_heads h WHERE h.content_version_id=j.content_version_id AND h.profile_fingerprint=j.profile_fingerprint
 ))
 ORDER BY identity`, args...)
	if err != nil {
		return "", fmt.Errorf("selecting derivative purge authority: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	digest := sha256.New()
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return "", err
		}
		identity := jsontext.Value(encoded)
		// PostgreSQL inserts whitespace between array elements. The identity
		// contains only catalog IDs and nulls; preserve SQLite's compact bytes.
		if err := identity.Compact(); err != nil {
			return "", fmt.Errorf("formatting derivative purge identity: %w", err)
		}
		_, _ = fmt.Fprintln(digest, string(identity))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
