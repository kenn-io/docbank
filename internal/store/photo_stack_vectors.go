package store

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"

	"go.kenn.io/docbank/internal/vectorindex"
)

// photoStackVectorReader opens the active image generations within a grouping transaction.
type photoStackVectorReader struct {
	indexes []*vectorindex.Generation
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

func (s *Store) newPhotoStackVectorReader(ctx context.Context, q metadataQuerier) (reader *photoStackVectorReader, err error) {
	rows, err := q.QueryContext(ctx, `SELECT h.generation_id FROM vector_index_heads h WHERE EXISTS(SELECT 1 FROM embedding_heads e WHERE e.vector_space_id=h.vector_space_id AND e.input_kind='image') ORDER BY h.vector_space_id`)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	reader = &photoStackVectorReader{}
	for _, id := range ids {
		stored, err := loadVectorIndexGenerationTx(ctx, q, id)
		if err != nil {
			return nil, err
		}
		index, err := vectorindex.OpenGeneration(bytes.NewReader(stored.Bytes), int64(len(stored.Bytes)))
		if err != nil {
			return nil, err
		}
		reader.indexes = append(reader.indexes, index)
	}
	return reader, nil
}

func (reader *photoStackVectorReader) hydrate(ctx context.Context, q metadataQuerier, frames map[string]photoStackFrame) error {
	if len(frames) == 0 {
		return nil
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
	err := queryInChunks(ctx, q, `SELECT a.asset_id,evr.vector_set_id,evr.input_id,evr.checksum,eig.source_version_id,eig.generation_id,eig.chunk_policy_fingerprint,eig.profile_fingerprint,eig.evidence_fingerprint,es.binding_id FROM photo_assets a JOIN photo_files f ON f.file_id=a.display_file_id JOIN nodes n ON n.id=f.node_id JOIN embedding_heads eh ON eh.content_version_id=n.current_version_id JOIN embedding_sets es ON es.embedding_set_id=eh.embedding_set_id JOIN embedding_input_generations eig ON eig.generation_id=es.input_generation_id JOIN embedding_vector_rows evr ON evr.vector_set_id=es.vector_set_id WHERE eh.input_kind='image' AND eh.vector_space_id=? AND `+currentImageEmbeddingSQL("es.content_version_id", "eig.evidence_fingerprint")+` AND a.asset_id IN (`, ids, func(rows *sql.Rows) error {
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
	slices.SortFunc(selected, func(a, b vectorindex.RowIdentity) int {
		return cmp.Or(cmp.Compare(a.SetID, b.SetID), cmp.Compare(a.InputKey, b.InputKey), cmp.Compare(a.InputChecksum, b.InputChecksum))
	})
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
