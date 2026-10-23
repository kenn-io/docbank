package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"go.kenn.io/docbank/internal/vectorindex"
)

// photoStackVectorReader reads one active image generation within a grouping transaction.
type photoStackVectorReader struct {
	id, space string
	index     *vectorindex.Generation
}

func (s *Store) hydratePhotoStackVectors(ctx context.Context, q metadataQuerier, frames map[string]photoStackFrame) error {
	if len(frames) == 0 {
		return nil
	}
	reader, err := s.newPhotoStackVectorReader(ctx, q)
	if err != nil {
		return err
	}
	return reader.hydrate(ctx, q, frames)
}

func (s *Store) newPhotoStackVectorReader(ctx context.Context, q metadataQuerier) (*photoStackVectorReader, error) {
	generationID, err := photoStackVectorGeneration(ctx, q)
	if err != nil {
		return nil, err
	}
	if generationID == "" {
		return &photoStackVectorReader{}, nil
	}
	index, err := s.photoStackVectors.Get(generationID, func() ([]byte, error) {
		stored, err := loadVectorIndexGenerationTx(ctx, q, generationID)
		return stored.Bytes, err
	})
	if err != nil {
		return nil, err
	}
	return &photoStackVectorReader{id: generationID, space: index.VectorSpaceID(), index: index}, nil
}

func (reader *photoStackVectorReader) hydrate(ctx context.Context, q metadataQuerier, frames map[string]photoStackFrame) error {
	if len(frames) == 0 {
		return nil
	}
	if reader.id == "" {
		var hasImages bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM embedding_heads WHERE input_kind='image') OR EXISTS(SELECT 1 FROM embedding_jobs WHERE input_kind='image')`).Scan(&hasImages); err != nil {
			return err
		}
		if !hasImages {
			return nil
		}
	}
	ids := make([]string, 0, len(frames))
	for id, frame := range frames {
		ids = append(ids, id)
		frame.ImageMissing = false
		frames[id] = frame
	}
	// Only queued work with an eligible current preview can improve coverage.
	err := queryInChunks(ctx, q, `SELECT DISTINCT a.asset_id FROM photo_assets a JOIN photo_files f ON f.file_id=a.display_file_id JOIN nodes n ON n.id=f.node_id JOIN embedding_jobs j ON j.content_version_id=n.current_version_id JOIN embedding_input_generations g ON g.generation_id=j.generation_id JOIN visual_preview_generations vp ON vp.content_version_id=n.current_version_id AND vp.checksum=g.evidence_fingerprint WHERE a.hidden_at IS NULL AND n.trashed_at IS NULL AND j.input_kind='image' AND j.state IN ('queued','retry_wait','running') AND EXISTS(SELECT 1 FROM processing_consent_grants cg JOIN current_processing_incarnation ci ON ci.incarnation_id=cg.incarnation_id WHERE cg.grant_id=j.authorization_grant_id AND cg.incarnation_id=j.authorization_incarnation_id AND cg.revocation_fence=j.authorization_revocation_fence AND (cg.expires_at IS NULL OR cg.expires_at>strftime('%Y-%m-%dT%H:%M:%fZ','now')) AND cg.revocation_fence=(SELECT COALESCE(MAX(cr.fence),0) FROM processing_consent_revocations cr WHERE cr.vault_uid=cg.vault_uid AND cr.incarnation_id=cg.incarnation_id AND cr.principal=cg.principal AND cr.scope=cg.scope)) AND (?='' OR j.vector_space_id=?) AND vp.state='ready' AND a.asset_id IN (`, ids, func(rows *sql.Rows) error {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		frame := frames[id]
		frame.ImageMissing = true
		frames[id] = frame
		return nil
	}, reader.space, reader.space)
	if err != nil {
		return err
	}
	slices.Sort(ids)
	identities := map[vectorindex.RowIdentity]string{}
	err = queryInChunks(ctx, q, `SELECT a.asset_id,evr.vector_set_id,evr.input_id,evr.checksum FROM photo_assets a JOIN photo_files f ON f.file_id=a.display_file_id JOIN nodes n ON n.id=f.node_id JOIN embedding_heads eh ON eh.content_version_id=n.current_version_id JOIN embedding_sets es ON es.embedding_set_id=eh.embedding_set_id JOIN embedding_input_generations eig ON eig.generation_id=es.input_generation_id JOIN embedding_vector_rows evr ON evr.vector_set_id=es.vector_set_id WHERE eh.input_kind='image' AND (?='' OR eh.vector_space_id=?) AND `+currentImageEmbeddingSQL("es.content_version_id", "eig.evidence_fingerprint")+` AND a.asset_id IN (`, ids, func(rows *sql.Rows) error {
		var id string
		var identity vectorindex.RowIdentity
		if err := rows.Scan(&id, &identity.SetID, &identity.InputKey, &identity.InputChecksum); err != nil {
			return err
		}
		identities[identity] = id
		frame := frames[id]
		frame.ImageMissing = true
		frames[id] = frame
		return nil
	}, reader.space, reader.space)
	if err != nil {
		return err
	}
	if reader.index == nil {
		return nil
	}
	selected := make([]vectorindex.RowIdentity, 0, len(identities))
	for identity := range identities {
		selected = append(selected, identity)
	}
	for identity, vector := range reader.index.RowVectors(selected) {
		id := identities[identity]
		frame := frames[id]
		frame.ImageVector = vector
		frame.ImageMissing = false
		frames[id] = frame
	}
	return nil
}

func photoStackVectorGeneration(ctx context.Context, q metadataQuerier) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT h.generation_id FROM vector_index_heads h WHERE EXISTS(SELECT 1 FROM embedding_heads e WHERE e.vector_space_id=h.vector_space_id AND e.input_kind='image') ORDER BY h.vector_space_id LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// PhotoStackVectorGeneration returns the active image generation used for grouping.
func (s *Store) PhotoStackVectorGeneration(ctx context.Context) (string, error) {
	var generation string
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		generation, err = photoStackVectorGeneration(ctx, tx)
		return err
	})
	return generation, err
}
