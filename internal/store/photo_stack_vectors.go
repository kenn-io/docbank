package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"slices"

	"go.kenn.io/docbank/internal/vectorindex"
)

// photoStackVectorReader holds the immutable image generations selected for one grouping request.
type photoStackVectorReader struct {
	indexes   []*vectorindex.Generation
	heads     map[string]string
	onHydrate func(int)
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

type photoStackOpenedGeneration struct {
	id    string
	index *vectorindex.Generation
}

func photoStackVectorHeads(ctx context.Context, q metadataQuerier) (heads map[string]string, err error) {
	rows, err := q.QueryContext(ctx, `SELECT h.vector_space_id,h.generation_id FROM vector_index_heads h JOIN embedding_vector_spaces s ON s.vector_space_id=h.vector_space_id WHERE s.metric IN ('cosine','dot_product') AND EXISTS(SELECT 1 FROM embedding_heads e WHERE e.vector_space_id=h.vector_space_id AND e.input_kind='image')`)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	heads = make(map[string]string)
	for rows.Next() {
		var space, id string
		if err := rows.Scan(&space, &id); err != nil {
			return nil, err
		}
		heads[space] = id
	}
	return heads, rows.Err()
}

func (s *Store) newPhotoStackVectorReader(ctx context.Context, q metadataQuerier) (*photoStackVectorReader, error) {
	heads, err := photoStackVectorHeads(ctx, q)
	if err != nil {
		return nil, err
	}
	reader := &photoStackVectorReader{heads: heads}
	s.photoStackVectorsMu.Lock()
	if s.photoStackVectors == nil {
		s.photoStackVectors = make(map[string]photoStackOpenedGeneration)
	}
	for space, opened := range s.photoStackVectors {
		if heads[space] != opened.id {
			delete(s.photoStackVectors, space)
		}
	}
	missing := make(map[string]string)
	for space, id := range heads {
		if opened := s.photoStackVectors[space]; opened.id == id {
			reader.indexes = append(reader.indexes, opened.index)
		} else {
			missing[space] = id
		}
	}
	s.photoStackVectorsMu.Unlock()
	for space, id := range missing {
		if s.photoStackGenerationLoadHook != nil {
			s.photoStackGenerationLoadHook()
		}
		stored, err := loadVectorIndexGenerationTx(ctx, q, id)
		if errors.Is(err, errUnusableVectorIndexGeneration) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		index, err := vectorindex.OpenGeneration(bytes.NewReader(stored.Bytes), int64(len(stored.Bytes)))
		if err != nil {
			// Indexes are disposable projections. Keep capture-gap grouping available
			// when an active generation is damaged.
			continue
		}
		s.photoStackVectorsMu.Lock()
		if opened := s.photoStackVectors[space]; opened.id == id {
			index = opened.index
		} else {
			s.photoStackVectors[space] = photoStackOpenedGeneration{id: id, index: index}
		}
		s.photoStackVectorsMu.Unlock()
		reader.indexes = append(reader.indexes, index)
	}
	return reader, nil
}

func (reader *photoStackVectorReader) hydrate(ctx context.Context, q metadataQuerier, frames map[string]photoStackFrame) error {
	if len(frames) == 0 {
		return nil
	}
	if reader.onHydrate != nil {
		reader.onHydrate(len(frames))
	}
	for _, index := range reader.indexes {
		if err := hydratePhotoStackVectorSpace(ctx, q, index, frames); err != nil {
			return err
		}
	}
	return nil
}

func hydratePhotoStackVectorSpace(ctx context.Context, q metadataQuerier, index *vectorindex.Generation, frames map[string]photoStackFrame) error {
	ids := make([]string, 0, len(frames))
	for id := range frames {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	identities := map[vectorindex.RowIdentity]string{}
	err := queryInChunks(ctx, q, `SELECT a.asset_id,evr.vector_set_id,evr.input_id,evr.checksum,eig.source_version_id,eig.generation_id,eig.chunk_policy_fingerprint,eig.profile_fingerprint,eig.evidence_fingerprint,es.binding_id FROM photo_assets a JOIN photo_files f ON f.file_id=a.display_file_id JOIN nodes n ON n.id=f.node_id JOIN embedding_heads eh ON eh.content_version_id=n.current_version_id JOIN embedding_sets es ON es.embedding_set_id=eh.embedding_set_id JOIN embedding_input_generations eig ON eig.generation_id=es.input_generation_id JOIN embedding_vector_rows evr ON evr.vector_set_id=es.vector_set_id WHERE eh.input_kind='image' AND eh.vector_space_id=? AND `+photoDescriptionEligibility+` AND EXISTS(SELECT 1 FROM visual_preview_generations image_preview WHERE image_preview.content_version_id=n.current_version_id AND image_preview.checksum=eig.evidence_fingerprint AND image_preview.state='ready') AND a.asset_id IN (`, ids, func(rows *sql.Rows) error {
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
	}, index.VectorSpaceID())
	if err != nil {
		return err
	}
	selected := make([]vectorindex.RowIdentity, 0, len(identities))
	for identity := range identities {
		selected = append(selected, identity)
	}
	// Multiple bindings may publish different vectors for one asset in this
	// space. Pick the lowest indexed row identity before hydrating its frame.
	slices.SortFunc(selected, vectorindex.CompareIdentity)
	vectors := index.RowVectors(selected)
	chosen := make(map[string][]float32)
	for _, identity := range selected {
		id := identities[identity]
		if vector, ok := vectors[identity]; ok && chosen[id] == nil {
			chosen[id] = vector
		}
	}
	for id, vector := range chosen {
		frame := frames[id]
		if frame.ImageVectors == nil {
			frame.ImageVectors = make(map[string][]float32)
		}
		frame.ImageVectors[index.VectorSpaceID()] = vector
		frames[id] = frame
	}
	return nil
}
