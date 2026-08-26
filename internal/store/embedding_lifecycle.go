package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type embeddingOrphanCollection struct {
	inputGenerations int
	vectorSets       int
	payloads         []string
}

// collectOrphanEmbeddingArtifactsTx is the shared terminal collection path
// for explicit purge and ordinary derivative GC. Sets own membership; active
// roots can retain either immutable artifact independently.
func collectOrphanEmbeddingArtifactsTx(
	ctx context.Context, tx *sql.Tx, asOf string,
) (_ embeddingOrphanCollection, retErr error) {
	var result embeddingOrphanCollection
	generationRows, err := tx.QueryContext(ctx, `DELETE FROM embedding_input_generations AS g
		WHERE NOT EXISTS (
			SELECT 1 FROM embedding_sets s WHERE s.input_generation_id=g.generation_id
		) AND NOT EXISTS (
			SELECT 1 FROM current_rendition_roots r
			WHERE r.target_kind='embedding_input_generation' AND r.target_id=g.generation_id
			  AND r.active=1 AND (r.expires_at IS NULL OR r.expires_at>?)
		) RETURNING COALESCE(generation_blob_hash,'')`, asOf)
	if err != nil {
		return result, fmt.Errorf("collecting orphan embedding input generations: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, generationRows.Close()) }()
	for generationRows.Next() {
		var blobHash string
		if err := generationRows.Scan(&blobHash); err != nil {
			return result, fmt.Errorf("scanning collected embedding input generation: %w", err)
		}
		result.inputGenerations++
		if blobHash != "" {
			result.payloads = append(result.payloads, blobHash)
		}
	}
	if err := generationRows.Err(); err != nil {
		return result, fmt.Errorf("collecting orphan embedding input generations: %w", err)
	}
	if err := generationRows.Close(); err != nil {
		return result, fmt.Errorf("closing collected embedding input generations: %w", err)
	}

	vectorRows, err := tx.QueryContext(ctx, `DELETE FROM embedding_vector_sets AS v
		WHERE NOT EXISTS (
			SELECT 1 FROM embedding_sets s WHERE s.vector_set_id=v.vector_set_id
		) AND NOT EXISTS (
			SELECT 1 FROM current_rendition_roots r
			WHERE r.active=1 AND (r.expires_at IS NULL OR r.expires_at>?) AND (
				(r.target_kind='embedding_vector_set' AND r.target_id=v.vector_set_id) OR
				(r.target_kind='embedding_payload' AND r.target_id=v.payload_blob_hash)
			)
		) RETURNING payload_blob_hash`, asOf)
	if err != nil {
		return result, fmt.Errorf("collecting orphan embedding vector sets: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, vectorRows.Close()) }()
	for vectorRows.Next() {
		var payloadHash string
		if err := vectorRows.Scan(&payloadHash); err != nil {
			return result, fmt.Errorf("scanning collected embedding vector set: %w", err)
		}
		result.vectorSets++
		result.payloads = append(result.payloads, payloadHash)
	}
	if err := vectorRows.Err(); err != nil {
		return result, fmt.Errorf("collecting orphan embedding vector sets: %w", err)
	}
	if err := vectorRows.Close(); err != nil {
		return result, fmt.Errorf("closing collected embedding vector sets: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM embedding_vector_spaces AS v
		WHERE NOT EXISTS (SELECT 1 FROM embedding_sets s WHERE s.vector_space_id=v.vector_space_id)
		  AND NOT EXISTS (SELECT 1 FROM embedding_vector_sets s WHERE s.vector_space_id=v.vector_space_id)`); err != nil {
		return result, fmt.Errorf("collecting orphan embedding vector spaces: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM current_rendition_roots
		WHERE (target_kind='embedding_input_generation' AND NOT EXISTS (
			SELECT 1 FROM embedding_input_generations g WHERE g.generation_id=target_id
		)) OR (target_kind='embedding_vector_set' AND NOT EXISTS (
			SELECT 1 FROM embedding_vector_sets v WHERE v.vector_set_id=target_id
		)) OR (target_kind='embedding_payload' AND NOT EXISTS (
			SELECT 1 FROM embedding_vector_sets v WHERE v.payload_blob_hash=target_id
		))`); err != nil {
		return result, fmt.Errorf("removing collected embedding roots: %w", err)
	}
	return result, nil
}
