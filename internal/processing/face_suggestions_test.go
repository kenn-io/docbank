package processing

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestFaceSuggestionsMatchClusterAndMask(t *testing.T) {
	t.Parallel()
	face := func(id string, values []float32) faceVector {
		return faceVector{Face: store.FaceSuggestionFace{PhotoFace: store.PhotoFace{FaceID: id, ContentVersionID: "00000000-0000-4000-8000-000000000010"}}, Space: "model", Values: values}
	}
	a := face("00000000-0000-4000-8000-000000000001", []float32{1, 0})
	a.Face.PersonID = "00000000-0000-4000-8000-000000000020"
	a.Face.PersonName = "Ada"
	a.Face.AssertionID = "named"
	b := face("00000000-0000-4000-8000-000000000002", []float32{.8, .6})
	c := face("00000000-0000-4000-8000-000000000003", []float32{-1, 0})
	d := face("00000000-0000-4000-8000-000000000004", []float32{-.8, -.6})
	suggestions, err := buildFaceSuggestions(t.Context(), []faceVector{d, c, b, a})
	require.NoError(t, err)
	require.Len(t, suggestions, 2)
	require.Equal(t, a.Face.PersonID, suggestions[0].SuggestedPersonID)
	require.EqualValues(t, 1, suggestions[0].OccurrenceCount)
	require.Empty(t, suggestions[1].SuggestedPersonID)
	require.EqualValues(t, 2, suggestions[1].OccurrenceCount)
	b.Face.RejectedPersonIDs = []string{a.Face.PersonID}
	c.Face.Dismissed = true
	suggestions, err = buildFaceSuggestions(t.Context(), []faceVector{a, b, c, d})
	require.NoError(t, err)
	require.Empty(t, suggestions)
}

func TestFaceSuggestionEvidenceSplitRetainsLastPair(t *testing.T) {
	t.Parallel()
	var vectors []faceVector
	for i := range 49 {
		vectors = append(vectors, faceVector{Face: store.FaceSuggestionFace{PhotoFace: store.PhotoFace{FaceID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), ContentVersionID: "00000000-0000-4000-8000-000000000100"}}, Space: "model", Values: []float32{1, 0}})
	}
	suggestions, err := buildFaceSuggestions(t.Context(), vectors)
	require.NoError(t, err)
	require.Len(t, suggestions, 2)
	require.EqualValues(t, 47, suggestions[0].OccurrenceCount)
	require.EqualValues(t, 2, suggestions[1].OccurrenceCount)
}
func TestFaceSuggestionCentroidAndActiveAssignment(t *testing.T) {
	t.Parallel()
	id := "00000000-0000-4000-8000-000000000010"
	vectors := []faceVector{
		{Face: store.FaceSuggestionFace{PhotoFace: store.PhotoFace{FaceID: "00000000-0000-4000-8000-000000000001", ContentVersionID: id, AssertionID: "named"}, PersonID: id, PersonName: "Ada"}, Space: "model", Values: []float32{.2, .9797959}},
		{Face: store.FaceSuggestionFace{PhotoFace: store.PhotoFace{FaceID: "00000000-0000-4000-8000-000000000002", ContentVersionID: id, AssertionID: "named"}, PersonID: id, PersonName: "Ada"}, Space: "model", Values: []float32{.2, -.9797959}},
		{Face: store.FaceSuggestionFace{PhotoFace: store.PhotoFace{FaceID: "00000000-0000-4000-8000-000000000003", ContentVersionID: id}}, Space: "model", Values: []float32{1, 0}},
	}
	suggestions, err := buildFaceSuggestions(t.Context(), vectors)
	require.NoError(t, err)
	require.Len(t, suggestions, 1)
	require.Equal(t, id, suggestions[0].SuggestedPersonID)
	vectors[0].Face.PersonID = ""
	vectors[1].Face.PersonID = ""
	suggestions, err = buildFaceSuggestions(t.Context(), vectors)
	require.NoError(t, err)
	require.Empty(t, suggestions, "retired people's labels must not become centroids")
}

// Preserve the exact leader algorithm and measure its cost on mostly distinct
// faces, rather than hiding the scaling cost behind duplicate vectors.
func BenchmarkFaceSuggestionsDistinct(b *testing.B) {
	for _, count := range []int{2000, 8000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			random := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: deterministic synthetic face vectors.
			vectors := make([]faceVector, count)
			for i := range vectors {
				values := make([]float32, 512)
				var norm float64
				for j := range values {
					values[j] = float32(random.Float64()*2 - 1)
					norm += float64(values[j]) * float64(values[j])
				}
				for j := range values {
					values[j] /= float32(math.Sqrt(norm))
				}
				vectors[i] = faceVector{Face: store.FaceSuggestionFace{PhotoFace: store.PhotoFace{FaceID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), ContentVersionID: "00000000-0000-4000-8000-000000000100"}}, Space: "model", Values: values}
			}
			b.ResetTimer()
			for b.Loop() {
				pass := newFaceSuggestionPass(vectors)
				batches := 0
				var longest time.Duration
				for {
					start := time.Now()
					done, err := pass.advance(b.Context(), faceSuggestionComparisonBatch)
					longest = max(longest, time.Since(start))
					batches++
					if err != nil {
						b.Fatal(err)
					}
					if done {
						break
					}
				}
				b.ReportMetric(float64(batches), "batches/op")
				b.ReportMetric(float64(longest.Microseconds())/1000, "max-batch-ms")
			}
		})
	}
}

func TestFaceSuggestionsHonorCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := buildFaceSuggestions(ctx, []faceVector{{Face: store.FaceSuggestionFace{PhotoFace: store.PhotoFace{FaceID: "00000000-0000-4000-8000-000000000001"}}, Space: "model", Values: []float32{1, 0}}})
	require.ErrorIs(t, err, context.Canceled)
}

func buildFaceSuggestions(ctx context.Context, vectors []faceVector) ([]store.PersonMatchCandidate, error) {
	pass := newFaceSuggestionPass(vectors)
	for {
		done, err := pass.advance(ctx, 1)
		if err != nil {
			return nil, err
		}
		if done {
			return pass.candidates()
		}
	}
}
