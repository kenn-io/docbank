package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
)

// DerivativePurgeFingerprint seals the selected immutable derivative identities
// and their current heads in one read snapshot. Unrelated source metadata and
// global lexical-generation replacements do not change this selection. Roots
// and physical reachability are still checked by PurgeDerivatives at execution.
func (s *Store) DerivativePurgeFingerprint(ctx context.Context, request PurgeRequest) (_ string, retErr error) {
	args := []any{request.All}
	for _, ids := range [][]string{request.ContentVersionIDs, request.AttachmentIDs, request.BuildIDs} {
		encoded, err := json.Marshal(ids)
		if err != nil {
			return "", err
		}
		args = append(args, string(encoded))
	}
	rows, err := s.db.QueryContext(ctx, `WITH
 versions AS (SELECT value AS id FROM json_each(?2)),
 attachments AS (SELECT value AS id FROM json_each(?3)),
 builds AS (SELECT value AS id FROM json_each(?4)),
 selected_attachments AS (
 SELECT a.* FROM rendition_attachments a
 WHERE ?1 OR a.content_version_id IN versions OR a.attachment_id IN attachments OR a.build_id IN builds
 ),
 selected_builds AS (
 SELECT build_id FROM rendition_builds
 WHERE ?1 OR build_id IN builds OR build_id IN (SELECT build_id FROM selected_attachments)
 OR (provider_operation_id='`+legacyPlainTextProvider+`' AND source_sha256 IN
 (SELECT blob_hash FROM content_versions WHERE version_id IN versions))
 ),
 selected_generations AS (
 SELECT g.generation_id FROM embedding_input_generations g
 LEFT JOIN rendition_attachments a ON a.attachment_id=g.attachment_id
 WHERE ?1 OR g.source_version_id IN versions OR g.attachment_id IN attachments OR a.build_id IN builds
 ),
 selected_sets AS (
 SELECT s.* FROM embedding_sets s
 JOIN embedding_input_generations g ON g.generation_id=s.input_generation_id
 LEFT JOIN rendition_attachments a ON a.attachment_id=g.attachment_id
 WHERE ?1 OR s.content_version_id IN versions OR g.attachment_id IN attachments OR a.build_id IN builds
 ),
 selected_heads AS (
 SELECT h.* FROM rendition_heads h JOIN selected_attachments a ON a.attachment_id=h.attachment_id
 )
 SELECT json_array('attachment',a.attachment_id,a.content_version_id,a.build_id,a.profile_fingerprint) AS identity
 FROM selected_attachments a
 UNION ALL SELECT json_array('build',build_id) FROM selected_builds
 UNION ALL SELECT json_array('head',content_version_id,profile_fingerprint,attachment_id) FROM selected_heads
 UNION ALL SELECT json_array('embedding_generation',generation_id) FROM selected_generations
 UNION ALL SELECT json_array('embedding_vector',vector_set_id) FROM embedding_vector_sets WHERE ?1
 UNION ALL SELECT json_array('embedding_set',embedding_set_id) FROM selected_sets
 UNION ALL SELECT json_array('embedding_head',h.content_version_id,h.binding_id,h.input_kind,h.embedding_set_id)
 FROM embedding_heads h JOIN selected_sets s ON s.embedding_set_id=h.embedding_set_id
 UNION ALL SELECT json_array('rendition_waiter',j.job_id,w.waiter_id,w.content_version_id,w.profile_fingerprint,w.attachment_id)
 FROM rendition_jobs j LEFT JOIN rendition_job_waiters w ON w.job_id=j.job_id
 WHERE ?1 OR j.job_id IN builds OR w.content_version_id IN versions OR w.attachment_id IN attachments
 UNION ALL SELECT json_array('embedding_job',j.job_id)
 FROM embedding_jobs j
 WHERE ?1 OR j.content_version_id IN versions OR j.generation_id IN selected_generations
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
		var identity string
		if err := rows.Scan(&identity); err != nil {
			return "", err
		}
		_, _ = fmt.Fprintln(digest, identity)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
