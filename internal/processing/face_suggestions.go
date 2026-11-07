package processing

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
)

// FaceMatchDistance follows Kit's expanded AuraFace acceptance corpus in docs/kennembed.md.
const FaceMatchDistance = 1.189402

type faceVector = store.FaceVector

const faceSuggestionComparisonBatch = 64_000

type faceSuggestionGroup struct {
	person, name, space string
	faces               []store.FaceSuggestionFace
	leader              []float32
}

type faceSuggestionPass struct {
	signature                      string
	incomplete                     bool
	vectors                        []faceVector
	centroids                      []store.FaceCentroid
	visibleCentroids               []store.FaceCentroid
	groups                         []faceSuggestionGroup
	known                          map[string]int
	face, centroid, group, closest int
	distance                       float64
}

func newFaceSuggestionPass(vectors []faceVector) *faceSuggestionPass {
	slices.SortFunc(vectors, func(a, b faceVector) int { return strings.Compare(a.Face.FaceID, b.Face.FaceID) })
	return &faceSuggestionPass{vectors: vectors, centroids: store.FacePersonCentroids(vectors, true), visibleCentroids: store.FacePersonCentroids(vectors, false), known: map[string]int{}, closest: -1, distance: FaceMatchDistance}
}

// advance preserves the exact leader order while releasing the maintenance gate
// after at most budget comparisons. No partial candidates are published.
func (p *faceSuggestionPass) advance(ctx context.Context, budget int) (bool, error) {
nextFace:
	for p.face < len(p.vectors) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		v := p.vectors[p.face]
		if v.Face.AssertionID != "" || v.Face.Dismissed {
			p.face++
			continue
		}
		centroids := p.centroids
		if !v.Face.Hidden {
			centroids = p.visibleCentroids
		}
		for p.centroid < len(centroids) {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if budget == 0 {
				return false, nil
			}
			budget--
			i := p.centroid
			p.centroid++
			c := centroids[i]
			if c.Space != v.Space || slices.Contains(v.Face.RejectedPersonIDs, c.Person) {
				continue
			}
			d := vectorindex.Euclidean(v.Values, c.Values)
			if d < p.distance || p.closest < 0 && d == p.distance {
				p.closest, p.distance = i, d
			}
		}
		if p.closest >= 0 {
			c := centroids[p.closest]
			key := c.Space + "\x00" + c.Person
			if v.Face.Hidden {
				key += "\x00hidden"
			}
			i, ok := p.known[key]
			if !ok {
				i = len(p.groups)
				p.known[key] = i
				p.groups = append(p.groups, faceSuggestionGroup{person: c.Person, name: c.Name, space: c.Space})
			}
			p.groups[i].faces = append(p.groups[i].faces, v.Face)
		} else {
			for p.group < len(p.groups) {
				if err := ctx.Err(); err != nil {
					return false, err
				}
				if budget == 0 {
					return false, nil
				}
				budget--
				i := p.group
				p.group++
				g := p.groups[i]
				if g.person == "" && g.space == v.Space && g.faces[0].Hidden == v.Face.Hidden && vectorindex.Euclidean(v.Values, g.leader) <= FaceMatchDistance {
					p.groups[i].faces = append(p.groups[i].faces, v.Face)
					p.face++
					p.centroid, p.group, p.closest, p.distance = 0, 0, -1, FaceMatchDistance
					continue nextFace
				}
			}
			p.groups = append(p.groups, faceSuggestionGroup{space: v.Space, leader: v.Values, faces: []store.FaceSuggestionFace{v.Face}})
		}
		p.face++
		p.centroid, p.group, p.closest, p.distance = 0, 0, -1, FaceMatchDistance
	}
	return true, nil
}

func (p *faceSuggestionPass) candidates() ([]store.PersonMatchCandidate, error) {
	var candidates []store.PersonMatchCandidate
	for _, g := range p.groups {
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

// rebuildFaceSuggestions publishes groups from every available vector. Its
// signature omits payload availability and queue capacity, so a pass that left
// either short is not cached.
func (service *Service) rebuildFaceSuggestions(ctx context.Context) error {
	service.faceSuggestionsMu.Lock()
	defer service.faceSuggestionsMu.Unlock()
	pass := service.faceSuggestionsPending
	if pass == nil {
		snapshot, err := service.catalog.FaceMatchingInputs(ctx)
		if err != nil {
			return err
		}
		if snapshot.Signature == service.faceSuggestionsSignature {
			return nil
		}
		vectors, unavailable := service.catalog.FaceMatchingVectors(ctx, service.blobs, snapshot, true)
		if unavailable != nil && !errors.Is(unavailable, store.ErrVectorSetUnavailable) {
			return unavailable
		}
		pass = newFaceSuggestionPass(vectors)
		pass.signature, pass.incomplete = snapshot.Signature, unavailable != nil
		service.faceSuggestionsPending = pass
	}
	done, err := pass.advance(ctx, faceSuggestionComparisonBatch)
	if err != nil || !done {
		return err
	}
	service.faceSuggestionsPending = nil
	candidates, err := pass.candidates()
	if err != nil {
		return err
	}
	if err := service.catalog.ReplaceFaceSuggestions(ctx, candidates); err != nil {
		return err
	}
	if pass.incomplete {
		return store.ErrVectorSetUnavailable
	}
	if _, full, err := service.catalog.OpenPersonCandidateCount(ctx); err != nil || full {
		return err
	}
	service.faceSuggestionsSignature = pass.signature
	return nil
}
func (service *Service) FaceSuggestionBackfill(ctx context.Context, logger *slog.Logger) error {
	settled, revision := ^uint64(0), uint64(0)
	lane := Backfill[string]{Name: "face-suggestions", Page: 1, IdleDelay: time.Second, WriteRevision: service.catalog.WriteRevision, Logger: logger, Mutate: service.gate.MutateContext,
		List: func(_ context.Context, after string, _ int) ([]string, error) {
			service.faceSuggestionsMu.Lock()
			pending := service.faceSuggestionsPending != nil
			service.faceSuggestionsMu.Unlock()
			if after != "" && !pending || settled == service.catalog.WriteRevision() {
				return nil, nil
			}
			return []string{"faces"}, nil
		}, Key: func(s string) string { return s },
		Process: func(ctx context.Context, _ string) error {
			service.faceSuggestionsMu.Lock()
			if service.faceSuggestionsPending == nil {
				revision = service.catalog.WriteRevision()
			}
			service.faceSuggestionsMu.Unlock()
			err := service.rebuildFaceSuggestions(ctx)
			service.faceSuggestionsMu.Lock()
			if err == nil && service.faceSuggestionsPending == nil {
				settled = revision
			}
			service.faceSuggestionsMu.Unlock()
			return err
		}}
	return lane.Run(ctx)
}
