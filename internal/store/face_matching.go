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
	Signature string
	Sets      []FaceSuggestionSet
	rejected  []string
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
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = s.faceMatchingInputsTx(ctx, tx)
		return err
	})
	return result, err
}
func (s *Store) faceMatchingInputsTx(ctx context.Context, tx *sql.Tx) (FaceMatchingSnapshot, error) {
	sets, err := s.faceSuggestionInputsTx(ctx, tx, "", "", nil)
	if err != nil {
		return FaceMatchingSnapshot{}, err
	}
	rejected, err := stringColumnTx(ctx, tx, "rejected face evidence", `SELECT actor_key || ':' || COALESCE(suggested_person_id,'') || ':' || evidence_sha256 FROM person_match_candidates WHERE reason='face_match' AND state='rejected' ORDER BY candidate_id`)
	if err != nil {
		return FaceMatchingSnapshot{}, err
	}
	snapshot := FaceMatchingSnapshot{Sets: sets, rejected: rejected}
	snapshot.Signature, err = snapshot.signature(nil)
	return snapshot, err
}

// signature projects current authority to the versions captured by a pass.
// A nil scope includes every version; an empty scope includes none.
func (snapshot FaceMatchingSnapshot) signature(versions map[string]bool) (string, error) {
	sets := make([]FaceSuggestionSet, 0, len(snapshot.Sets))
	for _, item := range snapshot.Sets {
		if versions == nil || versions[item.Set.ContentVersionID] {
			sets = append(sets, item)
		}
	}
	data, err := json.Marshal(struct {
		Sets     []FaceSuggestionSet
		Rejected []string
	}{sets, snapshot.rejected}, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	return LengthPrefixedSHA256(string(data)), nil
}

// faceRankingInputsTx keeps the source and every named contribution in the
// current read snapshot. Unnamed photos cannot change a person's centroid.
func (s *Store) faceRankingInputsTx(ctx context.Context, tx *sql.Tx, faceID, candidates string, args []any) (FaceMatchingSnapshot, error) {
	sets, err := s.faceSuggestionInputsTx(ctx, tx, faceID, candidates, args)
	if err != nil {
		return FaceMatchingSnapshot{}, err
	}
	return FaceMatchingSnapshot{Sets: sets}, nil
}

// FaceMatchingVectors reads exact immutable vectors for the current operation.
// Hidden membership is projected after the caller's source authorization.
func (s *Store) FaceMatchingVectors(ctx context.Context, reader RenditionBlobReader, snapshot FaceMatchingSnapshot, includeHidden bool, indexes []*vectorindex.Generation) ([]FaceVector, error) {
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
		decoded, err := s.faceVectorSet(ctx, reader, item.Set, indexes)
		if errors.Is(err, ErrVectorSetUnavailable) {
			unavailable = errors.Join(unavailable, err)
			continue
		}
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

func (s *Store) faceVectorSet(ctx context.Context, reader RenditionBlobReader, set EmbeddingSetRecord, indexes []*vectorindex.Generation) (document.VectorSetV1, error) {
	for _, index := range indexes {
		if index.VectorSpaceID() != set.VectorSpace.ID {
			continue
		}
		identities := make([]vectorindex.RowIdentity, len(set.VectorSet.rows))
		for i, row := range set.VectorSet.rows {
			identities[i] = vectorindex.RowIdentity{SetID: set.VectorSet.ID, InputKey: row.InputID, InputChecksum: row.Checksum}
		}
		rows := index.RowVectors(identities)
		if len(rows) == len(identities) {
			result := document.VectorSetV1{}
			for _, identity := range identities {
				result.InputKeys = append(result.InputKeys, identity.InputKey)
				result.Vectors = append(result.Vectors, rows[identity])
			}
			return result, nil
		}
	}
	data, err := s.ReadVectorIndexVectorSet(ctx, reader, VectorIndexMember{EmbeddingSetID: set.ID, VectorSetID: set.VectorSet.ID, PayloadBlobHash: set.VectorSet.PayloadBlobHash, PayloadSize: set.VectorSet.PayloadSize})
	if err != nil {
		return document.VectorSetV1{}, err
	}
	return ValidateReusableEmbeddingPayload(set, data)
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
