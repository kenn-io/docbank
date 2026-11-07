package processing

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
)

const FaceMatchDistance = 1.189402

type faceVector = store.FaceVector

func buildFaceSuggestions(vectors []faceVector) ([]store.PersonMatchCandidate, error) {
	slices.SortFunc(vectors, func(a, b faceVector) int { return strings.Compare(a.Face.FaceID, b.Face.FaceID) })
	centroids := store.FacePersonCentroids(vectors)
	type group struct {
		person, name, space string
		faces               []store.FaceSuggestionFace
		leader              []float32
	}
	var groups []group
	known := map[string]int{}
	for _, v := range vectors {
		if v.Face.AssertionID != "" || v.Face.Dismissed {
			continue
		}
		closest := -1
		distance := FaceMatchDistance
		for i, c := range centroids {
			if c.Space != v.Space || slices.Contains(v.Face.RejectedPersonIDs, c.Person) {
				continue
			}
			d := vectorindex.Euclidean(v.Values, c.Values)
			if d < distance || closest < 0 && d == distance {
				closest, distance = i, d
			}
		}
		if closest >= 0 {
			c := centroids[closest]
			key := c.Space + "\x00" + c.Person
			i, ok := known[key]
			if !ok {
				i = len(groups)
				known[key] = i
				groups = append(groups, group{person: c.Person, name: c.Name, space: c.Space})
			}
			groups[i].faces = append(groups[i].faces, v.Face)
			continue
		}
		selected := -1
		for i, g := range groups {
			if g.person == "" && g.space == v.Space && vectorindex.Euclidean(v.Values, g.leader) <= FaceMatchDistance {
				selected = i
				break
			}
		}
		if selected < 0 {
			selected = len(groups)
			groups = append(groups, group{space: v.Space, leader: v.Values})
		}
		groups[selected].faces = append(groups[selected].faces, v.Face)
	}
	var candidates []store.PersonMatchCandidate
	for _, g := range groups {
		if g.person == "" && len(g.faces) < 2 {
			continue
		}
		// 48 UUID occurrences fit the existing 16 KiB evidence limit.
		for start := 0; start < len(g.faces); {
			end := min(start+48, len(g.faces))
			if g.person == "" && len(g.faces)-end == 1 {
				end--
			}
			c, err := store.NewFaceCandidate(g.faces[start:end], g.person, g.name)
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, c)
			start = end
		}
	}
	return candidates, nil
}
func (service *Service) rebuildFaceSuggestions(ctx context.Context) error {
	service.faceSuggestionsMu.Lock()
	defer service.faceSuggestionsMu.Unlock()
	snapshot, err := service.catalog.FaceMatchingInputs(ctx)
	if err != nil {
		return err
	}
	if snapshot.Signature == service.faceSuggestionsSignature {
		return nil
	}
	vectors, err := service.catalog.FaceMatchingVectors(ctx, service.blobs, snapshot, true)
	if err != nil {
		return err
	}
	candidates, err := buildFaceSuggestions(vectors)
	if err != nil {
		return err
	}
	if err := service.catalog.ReplaceFaceSuggestions(ctx, snapshot.Signature, candidates); err != nil {
		return err
	}
	service.faceSuggestionsSignature = snapshot.Signature
	return nil
}
func (service *Service) FaceSuggestionBackfill(ctx context.Context, logger *slog.Logger) error {
	var settled uint64
	lane := Backfill[string]{Name: "face-suggestions", Page: 1, IdleDelay: time.Second, WriteRevision: service.catalog.WriteRevision, Logger: logger, Mutate: service.gate.MutateContext,
		List: func(_ context.Context, after string, _ int) ([]string, error) {
			if after != "" || settled == service.catalog.WriteRevision() {
				return nil, nil
			}
			return []string{"faces"}, nil
		}, Key: func(s string) string { return s },
		Process: func(ctx context.Context, _ string) error {
			revision := service.catalog.WriteRevision()
			err := service.rebuildFaceSuggestions(ctx)
			if err == nil {
				settled = revision
			}
			return err
		}}
	return lane.Run(ctx)
}
