package processing

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedlocal"
)

var ErrFaceModelUnavailable = errors.New("local face model unavailable")

type FaceReference struct {
	ContentVersionID string `json:"content_version_id" format:"uuid"`
	FaceID           string `json:"face_id" format:"uuid"`
}

type FaceClientFunc func(context.Context) (*embedclient.Client, error)

type FaceSimilaritySuggestion struct {
	State           string        `json:"state"`
	PersonID        string        `json:"person_id" format:"uuid"`
	PersonRevision  int64         `json:"person_revision"`
	DisplayName     string        `json:"display_name"`
	Distance        float64       `json:"distance"`
	Reference       FaceReference `json:"reference"`
	PreviewChecksum string        `json:"preview_checksum"`
}

type FaceSimilarityResult struct {
	Target          FaceReference              `json:"target"`
	FaceRevision    int64                      `json:"face_revision"`
	State           string                     `json:"state"`
	Model           string                     `json:"model"`
	ModelRevision   string                     `json:"model_revision"`
	Recipe          string                     `json:"recipe"`
	PreviewChecksum string                     `json:"preview_checksum"`
	Suggestions     []FaceSimilaritySuggestion `json:"suggestions" maxItems:"16"`
}

// LocalFaceClient discovers an existing face-capable Kit server. It does not
// install models, start a process, restart a shared server or select Ollama.
func LocalFaceClient(ctx context.Context) (*embedclient.Client, error) {
	origin, err := embedlocal.Endpoint(ctx, embedlocal.Options{Model: embedlocal.FaceModel, SkipOllama: true})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFaceModelUnavailable, err)
	}
	client, err := embedclient.New(embedclient.Options{
		Model:      embedconfig.Model{Name: embedclient.FaceModel, Dimensions: 512, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone},
		Deployment: embedconfig.Deployment{BaseURL: origin},
		Transport:  embedconfig.Transport{Timeout: 2 * time.Minute},
	})
	if err != nil {
		return nil, fmt.Errorf("configure local face client: %w", err)
	}
	return client, nil
}

type similarityFace struct {
	ref      FaceReference
	face     store.PhotoFace
	person   store.Person
	pixels   []byte
	checksum string
}

func readSimilarityFace(ctx context.Context, catalog *store.Store, ref FaceReference) (similarityFace, error) {
	faces, assertions, _, err := catalog.PhotoFacePersonNames(ctx, ref.ContentVersionID)
	if err != nil {
		return similarityFace{}, err
	}
	for _, face := range faces {
		if face.FaceID != ref.FaceID {
			continue
		}
		out := similarityFace{ref: ref, face: face}
		for _, assertion := range assertions {
			if assertion.AssertionID == face.AssertionID && assertion.Action == "assert" && assertion.Role == "depicted" {
				person, _, err := catalog.PersonByID(ctx, assertion.PersonID)
				if err != nil {
					return similarityFace{}, err
				}
				if person.State != "retired" {
					out.person = person
				}
			}
		}
		return out, nil
	}
	return similarityFace{}, store.ErrNotFound
}

// CompareFaces returns resemblance evidence for explicitly selected reference
// faces. Only existing reviewed assertions supply identities. Nothing is saved.
func CompareFaces(ctx context.Context, catalog *store.Store, blobs *blob.Store, load FaceClientFunc, target FaceReference, references []FaceReference) (FaceSimilarityResult, error) {
	result := FaceSimilarityResult{Target: target, State: "no_candidates", Suggestions: []FaceSimilaritySuggestion{},
		Model: embedclient.FaceModel, ModelRevision: embedclient.FaceRevision, Recipe: embedclient.FaceRecipe}
	if len(references) < 1 || len(references) > 16 {
		return result, store.ErrInvalidPhotoFace
	}
	selected, err := readSimilarityFace(ctx, catalog, target)
	if err != nil {
		return result, err
	}
	result.FaceRevision = selected.face.Revision
	if selected.face.Dismissed {
		result.State = "dismissed"
		return result, nil
	}
	if selected.person.PersonID != "" {
		return result, store.ErrInvalidPhotoFace
	}
	sources := []similarityFace{selected}
	seen := map[FaceReference]bool{target: true}
	for _, ref := range references {
		if seen[ref] {
			return result, store.ErrInvalidPhotoFace
		}
		seen[ref] = true
		source, err := readSimilarityFace(ctx, catalog, ref)
		if err != nil {
			return result, err
		}
		if source.person.PersonID == "" || source.face.Dismissed {
			return result, store.ErrInvalidPhotoFace
		}
		if !slices.Contains(selected.face.RejectedPersonIDs, source.person.PersonID) {
			sources = append(sources, source)
		}
	}
	if len(sources) == 1 {
		return result, nil
	}
	// Bound decoding and inference to one target plus sixteen references. Reuse
	// verified pixels when several boxes belong to the same exact version.
	previews := map[string]similarityFace{}
	for i := range sources {
		source := &sources[i]
		preview, ok := previews[source.ref.ContentVersionID]
		if !ok {
			preview.pixels, preview.checksum, _, err = strippedPhotoPreview(ctx, catalog, blobs, source.ref.ContentVersionID)
			if err != nil {
				return result, err
			}
			previews[source.ref.ContentVersionID] = preview
		}
		source.pixels, source.checksum = preview.pixels, preview.checksum
	}
	result.PreviewChecksum = sources[0].checksum
	client, err := load(ctx)
	if err != nil {
		return result, err
	}
	var targetVector []float32
	best := map[string]FaceSimilaritySuggestion{}
	for i, source := range sources {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		box := source.face.Box
		response, err := client.EmbedFaces(ctx, source.pixels, []embedclient.FaceBox{{X: box.X, Y: box.Y, Width: box.Width, Height: box.Height}})
		if err != nil {
			return result, fmt.Errorf("compare local face: %w", err)
		}
		vector := response.Data[0].Embedding // Kit validates count, indices, model and normalized vectors.
		if i == 0 {
			targetVector = vector
			if vector == nil {
				result.State = "unavailable"
				break
			}
			continue
		}
		if vector == nil {
			continue
		}
		var squared float64
		for j, v := range vector {
			delta := float64(v) - float64(targetVector[j])
			squared += delta * delta
		}
		distance := math.Sqrt(squared)
		previous, exists := best[source.person.PersonID]
		if !exists || distance < previous.Distance || distance == previous.Distance && source.ref.FaceID < previous.Reference.FaceID {
			best[source.person.PersonID] = FaceSimilaritySuggestion{State: "suggested", PersonID: source.person.PersonID,
				PersonRevision: source.person.Revision, DisplayName: source.person.DisplayName, Distance: distance,
				Reference: source.ref, PreviewChecksum: source.checksum}
		}
	}
	// A concurrent face edit, merge, retirement, replacement or visibility change
	// invalidates the whole comparison rather than publishing stale evidence.
	for _, source := range sources {
		current, err := readSimilarityFace(ctx, catalog, source.ref)
		if err != nil {
			return result, err
		}
		eligible, err := catalog.PhotoDescriptionEligible(ctx, source.ref.ContentVersionID)
		if err != nil {
			return result, err
		}
		view, _, err := photoDescriptionPreview(ctx, catalog, source.ref.ContentVersionID)
		if err != nil {
			return result, err
		}
		if !eligible || current.face.Revision != source.face.Revision || current.person.PersonID != source.person.PersonID ||
			current.person.Revision != source.person.Revision || view.Generation.Checksum != source.checksum {
			return result, store.ErrStaleRevision
		}
	}
	for _, suggestion := range best {
		result.Suggestions = append(result.Suggestions, suggestion)
	}
	slices.SortFunc(result.Suggestions, func(a, b FaceSimilaritySuggestion) int {
		if order := cmp.Compare(a.Distance, b.Distance); order != 0 {
			return order
		}
		return cmp.Compare(a.PersonID, b.PersonID)
	})
	if len(result.Suggestions) > 0 {
		result.State = "suggested"
	}
	return result, nil
}
