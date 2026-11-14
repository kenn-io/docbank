package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"maps"
	"slices"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

// FaceMatchDistance follows Kit's expanded AuraFace acceptance corpus in docs/kennembed.md.
const FaceMatchDistance = 1.189402

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

// HasFaceVectorHeads reports whether any face embeddings are retained.
func (s *Store) HasFaceVectorHeads(ctx context.Context) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM embedding_heads WHERE input_kind='face')`).Scan(&exists)
	return exists, err
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
	sets, err := s.faceSuggestionInputsTx(ctx, tx, "", "", "", nil)
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
	return requestDigest(struct {
		Sets     []FaceSuggestionSet
		Rejected []string
	}{sets, snapshot.rejected})
}

// faceRankingInputsTx keeps the source and every named contribution in the
// current read snapshot. Unnamed photos cannot change a person's centroid.
func (s *Store) faceRankingInputsTx(ctx context.Context, tx *sql.Tx, faceID, candidates string, args []any) (FaceMatchingSnapshot, error) {
	sets, err := s.faceSuggestionInputsTx(ctx, tx, faceID, candidates, "", args)
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

// authorizedFaceCandidatesTx checks the complete remaining group in the caller's
// snapshot. Stored evidence and decisions stay intact when a group is withheld.
func (s *Store) authorizedFaceCandidatesTx(ctx context.Context, tx *sql.Tx, reader RenditionBlobReader, ids []string) (map[string]bool, error) {
	allowed := make(map[string]bool, len(ids))
	for _, id := range ids {
		allowed[id] = true
	}
	if len(ids) == 0 {
		return allowed, nil
	}
	if _, err := s.hiddenSession(ctx, tx); err == nil {
		return allowed, nil
	} else if !errors.Is(err, ErrHiddenLocked) {
		return nil, err
	}
	var hidden bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_assets WHERE hidden_at IS NOT NULL)`).Scan(&hidden); err != nil {
		return nil, err
	}
	if !hidden {
		return allowed, nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	d := metadataJSONDialect{s.postgresSchema != ""}
	named := map[string]string{}
	concealed := map[string]bool{}
	people := map[string]bool{}
	err = queryRows(ctx, tx, `SELECT person_match_candidates.candidate_id,COALESCE(a.surviving_person_id,person_match_candidates.suggested_person_id),`+personCandidateVisibleSQL+` FROM person_match_candidates LEFT JOIN person_aliases a ON a.retired_person_id=person_match_candidates.suggested_person_id WHERE person_match_candidates.reason='face_match' AND person_match_candidates.suggested_person_id IS NOT NULL AND person_match_candidates.candidate_id IN (SELECT value FROM `+d.arrayValues("$1")+`)`, []any{string(encoded)}, func(rows *sql.Rows) error {
		var id, person string
		var visible bool
		if err := rows.Scan(&id, &person, &visible); err != nil {
			return err
		}
		named[id], people[person], allowed[id], concealed[id] = person, true, false, !visible
		return nil
	})
	if err != nil || len(named) == 0 {
		return allowed, err
	}
	targets := map[string][]string{}
	versions := map[string]bool{}
	err = queryRows(ctx, tx, `SELECT candidate.candidate_id,f.face_id,f.content_version_id FROM person_match_candidates candidate JOIN person_candidate_faces e ON e.candidate_id=candidate.candidate_id JOIN photo_faces f ON f.face_id=e.face_id AND f.content_version_id=e.content_version_id JOIN content_versions cv ON cv.version_id=f.content_version_id JOIN nodes n ON n.id=cv.node_id JOIN photo_files pf ON pf.node_id=n.id JOIN photo_assets a ON a.asset_id=pf.asset_id WHERE candidate.candidate_id IN (SELECT value FROM `+d.arrayValues("$1")+`) AND candidate.suggested_person_id IS NOT NULL AND f.assertion_id IS NULL AND a.kind='photo' AND a.excluded_at IS NULL AND a.hidden_at IS NULL AND `+currentPhotoSourcePredicate("pf", "n", "f.content_version_id")+` AND NOT `+faceSuggestionDecided("f.face_id", "f.content_version_id", "candidate.suggested_person_id"), []any{string(encoded)}, func(rows *sql.Rows) error {
		var id, face, version string
		if err := rows.Scan(&id, &face, &version); err != nil {
			return err
		}
		targets[id] = append(targets[id], face)
		versions[version] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	versionIDs, err := json.Marshal(slices.Collect(maps.Keys(versions)))
	if err != nil {
		return nil, err
	}
	personIDs, err := json.Marshal(slices.Collect(maps.Keys(people)))
	if err != nil {
		return nil, err
	}
	versionFilter := `h.content_version_id IN (SELECT value FROM ` + d.arrayValues("$1") + `) OR EXISTS(SELECT 1 FROM photo_faces named JOIN person_document_assertions pa ON pa.assertion_id=named.assertion_id AND pa.action='assert' WHERE named.content_version_id=h.content_version_id AND pa.person_id IN (SELECT value FROM ` + d.arrayValues("$2") + `))`
	sets, err := s.faceSuggestionInputsTx(ctx, tx, "", "", versionFilter, []any{string(versionIDs), string(personIDs)})
	if err != nil {
		return nil, err
	}
	sets = slices.DeleteFunc(sets, func(item FaceSuggestionSet) bool {
		if len(item.Faces) == 0 || item.Faces[0].Hidden {
			return true
		}
		if versions[item.Set.ContentVersionID] {
			return false
		}
		return !slices.ContainsFunc(item.Faces, func(face FaceSuggestionFace) bool { return people[face.PersonID] })
	})
	if reader == nil {
		return allowed, nil
	}
	heads, err := vectorHeads(ctx, tx, "face")
	if err != nil {
		return nil, err
	}
	indexes, err := s.openVectorReader(ctx, tx, heads)
	if err != nil {
		return nil, err
	}
	vectors, err := s.FaceMatchingVectors(ctx, reader, FaceMatchingSnapshot{Sets: sets}, false, indexes.indexes)
	if err != nil && !errors.Is(err, ErrVectorSetUnavailable) {
		return nil, err
	}
	byFace := map[string][]FaceVector{}
	present := map[string]bool{}
	for _, v := range vectors {
		byFace[v.Face.FaceID] = append(byFace[v.Face.FaceID], v)
		present[v.Space+"\x00"+v.Face.FaceID] = true
	}
	incomplete := map[string]bool{}
	for _, item := range sets {
		for _, row := range item.Set.VectorSet.rows {
			if present[item.Set.VectorSpace.ID+"\x00"+row.InputID] {
				continue
			}
			for _, face := range item.Faces {
				if face.FaceID == row.InputID && face.PersonID != "" {
					incomplete[item.Set.VectorSpace.ID+"\x00"+face.PersonID] = true
				}
			}
		}
	}
	centroids := map[string]FaceCentroid{}
	for _, c := range FacePersonCentroids(vectors, false) {
		centroids[c.Space+"\x00"+c.Person] = c
	}
	for id, person := range named {
		if concealed[id] || len(targets[id]) == 0 {
			continue
		}
		allowed[id] = true
		for _, face := range targets[id] {
			matched := false
			for _, v := range byFace[face] {
				key := v.Space + "\x00" + person
				if c, ok := centroids[key]; ok && !incomplete[key] && vectorindex.Euclidean(v.Values, c.Values) <= FaceMatchDistance {
					matched = true
				}
			}
			if !matched {
				allowed[id] = false
				break
			}
		}
	}
	return allowed, nil
}
