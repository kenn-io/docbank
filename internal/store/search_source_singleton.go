package store

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/docbank/document"
)

// canonicalOriginalSingleton is a validated, compact source fact, not a partial
// EmbeddingSetRecord. Its lifetime ends with this capture and its collector.
type canonicalOriginalSingleton struct {
	member                 originalSemanticMember
	source                 VectorIndexMember
	inputID, inputChecksum string
	row                    EmbeddingVectorRowRecord
}

// captureSemanticSearchSourceTx batches only canonical SQLite original-file
// singletons. The generic capture remains the authority for every other shape.
func (s *Store) captureSemanticSearchSourceTx(ctx context.Context, tx *sql.Tx, space string, collector *originalSemanticSnapshot) (VectorIndexSource, error) {
	legacy := func() (VectorIndexSource, error) {
		if collector != nil {
			return captureVectorIndexSourceObservedTx(ctx, tx, space, collector.observe)
		}
		return captureVectorIndexSourceTx(ctx, tx, space)
	}
	// PostgreSQL must never execute speculative SQLite SQL in its transaction.
	if s.postgresSchema != "" {
		return legacy()
	}
	if err := ctx.Err(); err != nil {
		return VectorIndexSource{}, err
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT semantic_source_singleton`); err != nil {
		return VectorIndexSource{}, err
	}
	source, facts, accepted, attemptErr := captureCanonicalOriginalSingletonsTx(ctx, tx, space, collector != nil)
	if attemptErr != nil || !accepted {
		cleanup := context.WithoutCancel(ctx)
		_, rollbackErr := tx.ExecContext(cleanup, `ROLLBACK TO semantic_source_singleton`)
		_, releaseErr := tx.ExecContext(cleanup, `RELEASE semantic_source_singleton`)
		if err := ctx.Err(); err != nil {
			return VectorIndexSource{}, errors.Join(err, attemptErr, rollbackErr, releaseErr)
		}
		if rollbackErr != nil || releaseErr != nil {
			return VectorIndexSource{}, errors.Join(attemptErr, rollbackErr, releaseErr)
		}
		// Only successful recovery permits legacy to choose error wrapping/priority.
		return legacy()
	}
	if _, err := tx.ExecContext(ctx, `RELEASE semantic_source_singleton`); err != nil {
		return VectorIndexSource{}, errors.Join(ctx.Err(), err)
	}
	if err := ctx.Err(); err != nil {
		return VectorIndexSource{}, err
	}
	if len(source.Members) == 0 {
		return VectorIndexSource{}, ErrNotFound
	}
	// No observer sees a speculative prefix that might later require replay.
	if collector != nil {
		for _, fact := range facts {
			if err := collector.observeSingleton(ctx, fact); err != nil {
				return VectorIndexSource{}, err
			}
		}
	}
	return source, nil
}

// The sole row driver is the requested heads, including historical, trashed and
// malformed members. LEFT JOINs never discard a candidate. Unique child ordinals
// cap the join before sorting: unsupported children replay legacy, rather than
// multiplying input/vector rows during a speculative query.
//
// Guards cover every metadata value scanned by the generic set/input/vector
// loaders, even fields not needed in the compact projection. Native storage
// classes must agree before Go string equality can replace SQL membership.
const canonicalOriginalSingletonQuery = `SELECT
 s.embedding_set_id,s.content_version_id,s.profile_fingerprint,s.binding_id,s.vector_space_id,
 vs.vector_set_id,vs.payload_blob_hash,vs.payload_size,
 r.row_id,r.input_id,r.dimensions,r.checksum,i.input_id,i.rendered_checksum,vs.manifest_checksum,
 COALESCE(n.id,0),COALESCE(n.kind,''),n.id IS NOT NULL,
 CASE WHEN
 typeof(h.embedding_set_id)='text' AND typeof(h.content_version_id)='text'
 AND typeof(h.profile_fingerprint)='text' AND typeof(h.binding_id)='text'
 AND typeof(h.input_kind)='text' AND typeof(h.vector_space_id)='text'
 AND typeof(s.embedding_set_id)='text' AND typeof(s.vault_uid)='text'
 AND typeof(s.binding_id)='text' AND typeof(s.input_kind)='text'
 AND typeof(s.content_version_id)='text' AND typeof(s.profile_fingerprint)='text'
 AND typeof(s.embedding_input_fingerprint)='text' AND typeof(s.vector_space_id)='text'
 AND typeof(s.input_generation_id)='text' AND typeof(s.vector_set_id)='text'
 AND typeof(s.created_at)='text'
 AND typeof(g.generation_id)='text' AND typeof(g.generation_blob_hash) IN ('text','null')
 AND typeof(g.generation_encoded_size)='integer' AND typeof(g.generation_checksum)='text'
 AND typeof(g.source_version_id)='text' AND typeof(g.profile_fingerprint)='text'
 AND typeof(g.evidence_fingerprint)='text' AND typeof(g.tokenizer_fingerprint)='text'
 AND typeof(g.chunk_policy_fingerprint)='text' AND typeof(g.formatter_fingerprint)='text'
 AND typeof(g.attachment_context_fingerprint)='text' AND g.attachment_id IS NULL
 AND typeof(g.input_count)='integer' AND typeof(g.created_at)='text'
 AND typeof(i.generation_id)='text' AND typeof(i.input_id)='text'
 AND typeof(i.input_order)='integer' AND typeof(i.rendered_checksum)='text'
 AND typeof(vs.vector_set_id)='text' AND typeof(vs.contract_version)='text'
 AND typeof(vs.vector_space_id)='text' AND typeof(vs.payload_blob_hash)='text'
 AND typeof(vs.payload_size)='integer' AND typeof(vs.payload_checksum)='text'
 AND typeof(vs.manifest_checksum)='text' AND typeof(vs.row_count)='integer'
 AND typeof(vs.dimensions)='integer'
 AND typeof(r.vector_set_id)='text' AND typeof(r.row_id)='text'
 AND typeof(r.row_order)='integer' AND typeof(r.input_id)='text'
 AND typeof(r.dimensions)='integer' AND typeof(r.checksum)='text'
 AND typeof(v.version_id)='text' AND typeof(v.node_id)='integer'
 AND (n.id IS NULL OR (typeof(n.id)='integer' AND typeof(n.kind)='text'))
 AND h.embedding_set_id=s.embedding_set_id AND h.content_version_id=s.content_version_id
 AND h.profile_fingerprint=s.profile_fingerprint AND h.binding_id=s.binding_id
 AND h.input_kind=s.input_kind AND h.vector_space_id=s.vector_space_id
 AND s.input_kind='original_file' AND vs.vector_space_id=s.vector_space_id
 AND g.source_version_id=s.content_version_id AND g.profile_fingerprint=s.profile_fingerprint
 AND g.input_count=1 AND vs.row_count=1
 AND NOT EXISTS (SELECT 1 FROM embedding_generation_inputs extra
                 WHERE extra.generation_id=g.generation_id
                 AND (typeof(extra.input_order)!='integer' OR extra.input_order<>0))
 AND NOT EXISTS (SELECT 1 FROM embedding_vector_rows extra
                 WHERE extra.vector_set_id=vs.vector_set_id
                 AND (typeof(extra.row_order)!='integer' OR extra.row_order<>0))
 THEN 1 ELSE 0 END
 FROM embedding_heads h
 LEFT JOIN embedding_sets s ON s.embedding_set_id=h.embedding_set_id
 LEFT JOIN embedding_input_generations g ON g.generation_id=s.input_generation_id
 LEFT JOIN embedding_generation_inputs i ON i.generation_id=g.generation_id AND i.input_order=0
 LEFT JOIN embedding_vector_sets vs ON vs.vector_set_id=s.vector_set_id
 LEFT JOIN embedding_vector_rows r ON r.vector_set_id=vs.vector_set_id AND r.row_order=0
 LEFT JOIN content_versions v ON v.version_id=s.content_version_id
 LEFT JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id AND n.trashed_at IS NULL
 WHERE h.vector_space_id=? ORDER BY h.embedding_set_id`

func captureCanonicalOriginalSingletonsTx(ctx context.Context, tx *sql.Tx, space string, collect bool) (_ VectorIndexSource, _ []canonicalOriginalSingleton, _ bool, retErr error) {
	rows, err := tx.QueryContext(ctx, canonicalOriginalSingletonQuery, space)
	if err != nil {
		return VectorIndexSource{}, nil, false, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	source := VectorIndexSource{VectorSpaceID: space}
	var facts []canonicalOriginalSingleton
	var seen bool
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return VectorIndexSource{}, nil, false, err
		}
		seen = true
		var fact canonicalOriginalSingleton
		var manifest string
		var canonical bool
		if err := rows.Scan(&fact.source.EmbeddingSetID, &fact.member.version, &fact.member.profile, &fact.member.binding, &fact.member.space,
			&fact.source.VectorSetID, &fact.source.PayloadBlobHash, &fact.source.PayloadSize,
			&fact.row.RowID, &fact.row.InputID, &fact.row.Dimensions, &fact.row.Checksum, &fact.inputID, &fact.inputChecksum, &manifest,
			&fact.member.live.nodeID, &fact.member.live.kind, &fact.member.live.current, &canonical); err != nil {
			return VectorIndexSource{}, nil, false, err
		}
		if !canonical || fact.member.space != space {
			return VectorIndexSource{}, nil, false, nil
		}
		// Validate the real row identity/dimensions/checksum before live eligibility,
		// including historical/trash members that never enter the source manifest.
		if embeddingVectorManifestChecksum([]EmbeddingVectorRowRecord{fact.row}) != manifest {
			return VectorIndexSource{}, nil, false, nil
		}
		fact.member.kind = document.EmbeddingInputOriginalFile
		fact.member.headMatches = true
		if fact.member.live.current {
			source.Members = append(source.Members, fact.source)
		}
		if collect {
			facts = append(facts, fact)
		}
	}
	if err := rows.Err(); err != nil {
		return VectorIndexSource{}, nil, false, err
	}
	if err := rows.Close(); err != nil {
		return VectorIndexSource{}, nil, false, err
	}
	if seen {
		// Actual spaces must all match the request to reach here. Strict descriptor
		// decoding is fresh in this snapshot; failures replay legacy for exact priority.
		if _, err := loadVectorSpaceTx(ctx, tx, space); err != nil {
			return VectorIndexSource{}, nil, false, err
		}
	}
	if len(source.Members) != 0 {
		source.ManifestChecksum = vectorIndexSourceChecksum(source.Members)
	}
	return source, facts, true, nil
}
