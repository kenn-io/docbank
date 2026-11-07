package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/docbank/internal/vectorindex"
)

func (s *Store) peopleForFaceTx(ctx context.Context, tx *sql.Tx, options PeopleSearchOptions, candidates string, args []any, afterName, afterID string, limit int, visibleOnly, hideLocked bool, prefix string) ([]PersonSearchResult, error) {
	if validateUUIDv4(options.FaceID) != nil || options.FaceReader == nil {
		return nil, ErrInvalidPhotoFace
	}
	var version string
	if err := tx.QueryRowContext(ctx, `SELECT content_version_id FROM photo_faces WHERE face_id=$1`, options.FaceID).Scan(&version); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if err := s.facePhotoQuery(ctx, tx, version); err != nil {
		return nil, err
	}
	snapshot, err := s.faceRankingInputsTx(ctx, tx, options.FaceID, candidates, args)
	if err != nil {
		return nil, err
	}
	_, hiddenErr := s.hiddenSession(ctx, tx)
	if hiddenErr != nil && !errors.Is(hiddenErr, ErrHiddenLocked) {
		return nil, hiddenErr
	}
	vectors, err := s.FaceMatchingVectors(ctx, options.FaceReader, snapshot, hiddenErr == nil)
	if err != nil && !errors.Is(err, ErrVectorSetUnavailable) {
		return nil, err
	}
	var source []float32
	var space, generationID string
	for _, v := range vectors {
		if v.Face.FaceID == options.FaceID {
			source = v.Values
			space = v.Space
		}
	}
	for _, item := range snapshot.Sets {
		for _, input := range item.Set.InputGeneration.Inputs {
			if input.ID == options.FaceID && item.Set.VectorSpace.ID == space {
				generationID = item.Set.InputGeneration.ID
			}
		}
	}
	if len(source) == 0 {
		if strings.Contains(options.AfterRecent, "#") {
			return nil, ErrStaleRevision
		}
		if options.RecentFirst {
			return peopleRecentTx(ctx, tx, candidates, args, afterName, afterID, options.AfterRecent, limit, visibleOnly, hideLocked, options.PhotoSummary, prefix)
		}
		return peopleByDisplayNameTx(ctx, tx, candidates, args, afterName, afterID, limit, visibleOnly, hideLocked)
	}
	var people []PersonSearchResult
	if options.RecentFirst {
		people, err = peopleRecentTx(ctx, tx, candidates, args, "", "", "", math.MaxInt32, visibleOnly, hideLocked, options.PhotoSummary, prefix)
	} else {
		people, err = peopleByDisplayNameTx(ctx, tx, candidates, args, "", "", math.MaxInt32, visibleOnly, hideLocked)
	}
	if err != nil {
		return nil, err
	}
	distances := map[string]float64{}
	for _, centroid := range FacePersonCentroids(vectors, hiddenErr == nil) {
		if centroid.Space == space {
			distances[centroid.Person] = vectorindex.Euclidean(source, centroid.Values)
		}
	}
	distance := func(id string) float64 {
		d, ok := distances[id]
		if !ok {
			return math.Inf(1)
		}
		return d
	}
	best := map[string]float64{}
	base := map[string]int{}
	for i, p := range people {
		if _, ok := base[p.DisplayNameFolded]; !ok {
			base[p.DisplayNameFolded] = i
		}
		d := distance(p.PersonID)
		if old, ok := best[p.DisplayNameFolded]; !ok || d < old {
			best[p.DisplayNameFolded] = d
		}
	}
	slices.SortStableFunc(people, func(a, b PersonSearchResult) int {
		if a.DisplayNameFolded != b.DisplayNameFolded {
			da, db := best[a.DisplayNameFolded], best[b.DisplayNameFolded]
			if da < db {
				return -1
			}
			if da > db {
				return 1
			}
			return base[a.DisplayNameFolded] - base[b.DisplayNameFolded]
		}
		da, db := distance(a.PersonID), distance(b.PersonID)
		if da < db {
			return -1
		}
		if da > db {
			return 1
		}
		return 0
	})
	// Carry the model identity with the ranked continuation so a replaced face
	// vector cannot silently continue a different order.
	binding := space + "/" + generationID + "/" + options.FaceID
	start := 0
	if afterID != "" {
		cursorBinding, position, ok := strings.Cut(options.AfterRecent, "#")
		if !ok || cursorBinding != binding {
			return nil, ErrStaleRevision
		}
		index, err := strconv.Atoi(position)
		if err != nil || index < 0 || index >= len(people) || people[index].PersonID != afterID || people[index].DisplayNameFolded != afterName {
			return nil, ErrStaleRevision
		}
		start = index + 1
	}
	end := min(start+limit, len(people))
	for i := start; i < end; i++ {
		people[i].RecentRank = binding + "#" + strconv.Itoa(i)
	}
	return people[start:end], nil
}
