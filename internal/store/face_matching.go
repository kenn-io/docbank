package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

type FaceMatchingSnapshot struct {
	Signature        string
	Sets             []FaceSuggestionSet
	ReferencedSetIDs []string
}
type FaceVector struct {
	Face   FaceSuggestionFace
	Space  string
	Values []float32
}
type FaceCentroid struct {
	Person, Name, Space string
	Values              []float32
}

// FaceMatchingInputs captures the authority used by both grouping and ranking.
// Candidate output is absent: publishing suggestions cannot invalidate its input.
func (s *Store) FaceMatchingInputs(ctx context.Context) (FaceMatchingSnapshot, error) {
	var result FaceMatchingSnapshot
	err := s.withReadTx(ctx, func(tx *sql.Tx) error { var err error; result, err = s.faceMatchingInputsTx(ctx, tx); return err })
	return result, err
}
func (s *Store) faceMatchingInputsTx(ctx context.Context, tx *sql.Tx) (FaceMatchingSnapshot, error) {
	sets, err := s.faceSuggestionInputsTx(ctx, tx, "")
	if err != nil {
		return FaceMatchingSnapshot{}, err
	}
	rejected, err := stringColumnTx(ctx, tx, "rejected face evidence", `SELECT actor_key || ':' || COALESCE(suggested_person_id,'') || ':' || evidence_sha256 FROM person_match_candidates WHERE reason='face_match' AND state='rejected' ORDER BY candidate_id`)
	if err != nil {
		return FaceMatchingSnapshot{}, err
	}
	data, err := json.Marshal(struct {
		Sets     []FaceSuggestionSet
		Rejected []string
	}{sets, rejected}, json.Deterministic(true))
	if err != nil {
		return FaceMatchingSnapshot{}, err
	}
	return FaceMatchingSnapshot{Signature: LengthPrefixedSHA256(string(data)), Sets: sets}, nil
}

// faceRankingInputsTx keeps the source and every named contribution in the
// current read snapshot. Unnamed photos cannot change a person's centroid.
func (s *Store) faceRankingInputsTx(ctx context.Context, tx *sql.Tx, faceID string) (FaceMatchingSnapshot, error) {
	sets, err := s.faceSuggestionInputsTx(ctx, tx, faceID)
	if err != nil {
		return FaceMatchingSnapshot{}, err
	}
	references, err := stringColumnTx(ctx, tx, "face payload references", `SELECT embedding_set_id FROM embedding_heads WHERE input_kind='face'`)
	return FaceMatchingSnapshot{Sets: sets, ReferencedSetIDs: references}, err
}

// FaceMatchingVectors keeps only currently referenced immutable payloads. Hidden
// membership is projected for each caller, after its source authorization.
func (s *Store) FaceMatchingVectors(ctx context.Context, reader RenditionBlobReader, snapshot FaceMatchingSnapshot, includeHidden bool) ([]FaceVector, error) {
	s.faceVectorsMu.Lock()
	defer s.faceVectorsMu.Unlock()
	live := make(map[string]bool, len(snapshot.Sets))
	for _, item := range snapshot.Sets {
		live[item.Set.ID] = true
	}
	// A ranking projection must preserve payloads still referenced by grouping.
	for _, id := range snapshot.ReferencedSetIDs {
		live[id] = true
	}
	for id := range s.faceVectors {
		if !live[id] {
			delete(s.faceVectors, id)
		}
	}
	if s.faceVectors == nil {
		s.faceVectors = map[string]document.VectorSetV1{}
	}
	var vectors []FaceVector
	var unavailable error
	seen := map[string]bool{}
	for _, item := range snapshot.Sets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !includeHidden && len(item.Faces) > 0 && item.Faces[0].Hidden {
			continue
		}
		decoded, ok := s.faceVectors[item.Set.ID]
		if !ok {
			data, err := s.ReadVectorIndexVectorSet(ctx, reader, VectorIndexMember{EmbeddingSetID: item.Set.ID, VectorSetID: item.Set.VectorSet.ID, PayloadBlobHash: item.Set.VectorSet.PayloadBlobHash, PayloadSize: item.Set.VectorSet.PayloadSize})
			if errors.Is(err, ErrVectorSetUnavailable) {
				if unavailable == nil {
					unavailable = err
				}
				continue
			}
			if err != nil {
				return nil, err
			}
			decoded, err = ValidateReusableEmbeddingPayload(item.Set, data)
			if err != nil {
				return nil, err
			}
			s.faceVectors[item.Set.ID] = decoded
		}
		faces := make(map[string]FaceSuggestionFace, len(item.Faces))
		for _, face := range item.Faces {
			faces[face.FaceID] = face
		}
		for i, id := range decoded.InputKeys {
			face, ok := faces[id]
			key := item.Set.VectorSpace.ID + "\x00" + id
			if ok && !seen[key] {
				seen[key] = true
				vectors = append(vectors, FaceVector{face, item.Set.VectorSpace.ID, decoded.Vectors[i]})
			}
		}
	}
	return vectors, unavailable
}
func FacePersonCentroids(vectors []FaceVector) []FaceCentroid {
	groups := map[string][]FaceVector{}
	for _, v := range vectors {
		if v.Face.PersonID != "" {
			key := v.Space + "\x00" + v.Face.PersonID
			groups[key] = append(groups[key], v)
		}
	}
	result := make([]FaceCentroid, 0, len(groups))
	for _, group := range groups {
		rows := make([][]float32, len(group))
		for i, v := range group {
			rows[i] = v.Values
		}
		result = append(result, FaceCentroid{group[0].Face.PersonID, group[0].Face.PersonName, group[0].Space, vectorindex.UnitCentroid(rows)})
	}
	slices.SortFunc(result, func(a, b FaceCentroid) int {
		if c := strings.Compare(a.Person, b.Person); c != 0 {
			return c
		}
		return strings.Compare(a.Space, b.Space)
	})
	return result
}
