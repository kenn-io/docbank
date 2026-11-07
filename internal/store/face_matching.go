package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/docbank/internal/vectorindex"
)

type FaceMatchingSnapshot struct {
	Signature string
	Sets      []FaceSuggestionSet
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
	return FaceMatchingSnapshot{Sets: sets}, nil
}

// FaceMatchingVectors decodes immutable payloads for the current operation.
// Hidden membership is projected after the caller's source authorization.
func (s *Store) FaceMatchingVectors(ctx context.Context, reader RenditionBlobReader, snapshot FaceMatchingSnapshot, includeHidden bool) ([]FaceVector, error) {
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
		data, err := s.ReadVectorIndexVectorSet(ctx, reader, VectorIndexMember{EmbeddingSetID: item.Set.ID, VectorSetID: item.Set.VectorSet.ID, PayloadBlobHash: item.Set.VectorSet.PayloadBlobHash, PayloadSize: item.Set.VectorSet.PayloadSize})
		if errors.Is(err, ErrVectorSetUnavailable) {
			unavailable = fmt.Errorf("%w", cmp.Or(unavailable, err))
			continue
		}
		if err != nil {
			return nil, err
		}
		decoded, err := ValidateReusableEmbeddingPayload(item.Set, data)
		if err != nil {
			return nil, err
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
func FacePersonCentroids(vectors []FaceVector, includeHidden bool) []FaceCentroid {
	groups := map[string][]FaceVector{}
	for _, v := range vectors {
		if v.Face.PersonID != "" && (includeHidden || !v.Face.Hidden) {
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
