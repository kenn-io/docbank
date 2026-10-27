package store

import (
	"cmp"
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
	if reader.index == nil {
		return nil
	}
	ids := make([]string, 0, len(frames))
	for id := range frames {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	identities := map[vectorindex.RowIdentity]string{}
	err := queryInChunks(ctx, q, `SELECT a.asset_id,evr.vector_set_id,evr.input_id,evr.checksum,eig.source_version_id,eig.generation_id,eig.chunk_policy_fingerprint,eig.profile_fingerprint,eig.evidence_fingerprint,es.binding_id FROM photo_assets a JOIN photo_files f ON f.file_id=a.display_file_id JOIN nodes n ON n.id=f.node_id JOIN embedding_heads eh ON eh.content_version_id=n.current_version_id JOIN embedding_sets es ON es.embedding_set_id=eh.embedding_set_id JOIN embedding_input_generations eig ON eig.generation_id=es.input_generation_id JOIN embedding_vector_rows evr ON evr.vector_set_id=es.vector_set_id WHERE eh.input_kind='image' AND (?='' OR eh.vector_space_id=?) AND `+currentImageEmbeddingSQL("es.content_version_id", "eig.evidence_fingerprint")+` AND a.asset_id IN (`, ids, func(rows *sql.Rows) error {
		var id string
		var identity vectorindex.RowIdentity
		var record EmbeddingInputGenerationRecord
		var bindingID string
		if err := rows.Scan(&id, &identity.SetID, &identity.InputKey, &identity.InputChecksum, &record.SourceVersionID, &record.ID, &record.ChunkPolicyFingerprint, &record.ProcessingProfileFingerprint, &record.EvidenceFingerprint, &bindingID); err != nil {
			return err
		}
		if validateImageEmbeddingGeneration(record, bindingID) == nil {
			identities[identity] = id
		}
		return nil
	}, reader.space, reader.space)
	if err != nil {
		return err
	}
	selected := make([]vectorindex.RowIdentity, 0, len(identities))
	for identity := range identities {
		selected = append(selected, identity)
	}
	// Multiple bindings may publish different vectors for one asset in this
	// space. Pick the lowest indexed row identity before hydrating its frame.
	slices.SortFunc(selected, func(a, b vectorindex.RowIdentity) int {
		return cmp.Or(cmp.Compare(a.SetID, b.SetID), cmp.Compare(a.InputKey, b.InputKey), cmp.Compare(a.InputChecksum, b.InputChecksum))
	})
	vectors := reader.index.RowVectors(selected)
	chosen := make(map[string][]float32)
	for _, identity := range selected {
		id := identities[identity]
		if vector, ok := vectors[identity]; ok && chosen[id] == nil {
			chosen[id] = vector
		}
	}
	for id, vector := range chosen {
		frame := frames[id]
		frame.ImageVector = vector
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
