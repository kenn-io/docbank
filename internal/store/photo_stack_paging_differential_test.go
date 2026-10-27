package store

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestReviewImageStacksAgreeForNontransitiveSimilarity(t *testing.T) {
	t.Parallel()
	f := newImageEmbeddingCatalogFixture(t)
	run, err := f.s.BeginIngest(t.Context(), "cli", "synthetic nontransitive frames")
	require.NoError(t, err)
	index := newCursorIndexFixture(f)
	var ids []string
	for i, vector := range [][]float64{{1, 0, 0, 0, 0, 0, 0, 0}, {.96, .28, 0, 0, 0, 0, 0, 0}, {.96, -.28, 0, 0, 0, 0, 0, 0}} {
		stamp := time.Date(2026, 1, 1, 0, 0, i*20, 0, time.UTC).Format(time.RFC3339)
		id := stackPhoto(t, f.s, run, fmt.Sprintf("/nontransitive/%d.jpg", i), stamp, document.SourceMetadataPrecisionSecond)
		ids = append(ids, id)
		index.publish(t, id, vector)
	}
	index.generation(t, "nontransitive-frames")
	coverage, err := f.s.PhotoAssetForNode(t.Context(), f.node.ID)
	require.NoError(t, err)
	groups, unavailable, _, incomplete, err := f.s.PhotoStackGroups(t.Context(), ids, false, "")
	require.NoError(t, err)
	require.Empty(t, unavailable)
	require.False(t, incomplete)
	require.Equal(t, [][]string{ids[:2]}, groups)
	for _, direction := range []string{"asc", "desc"} {
		for _, size := range []int{1, 50, 250} {
			t.Run(fmt.Sprintf("%s/%d", direction, size), func(t *testing.T) {
				request := captureRequest(t, direction)
				request.PageSize = size
				items, stacks := stackPages(t, f.s, request)
				require.Equal(t, [][]string{ids[:2]}, stacks)
				require.ElementsMatch(t, append(slices.Clone(ids), coverage.ID), slices.Concat(items...))
			})
		}
	}
}

func TestPhotoStackRandomizedPagingMatchesCull(t *testing.T) {
	t.Parallel()
	for _, seed := range []uint64{1, 42, 1030} {
		t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
			f := newImageEmbeddingCatalogFixture(t)
			run, err := f.s.BeginIngest(t.Context(), "cli", "synthetic mixed-vector frames")
			require.NoError(t, err)
			index := newCursorIndexFixture(f)
			random := rand.New(rand.NewPCG(seed, 839)) //nolint:gosec // Fixed seeds reproduce synthetic grouping cases.
			stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			var ids []string
			for i := range 60 {
				stamp = stamp.Add(time.Duration([]int{1, 2, 10, 20, 40, 61}[random.IntN(6)]) * time.Second)
				id := stackPhoto(t, f.s, run, fmt.Sprintf("/mixed/%d.jpg", i), stamp.Format(time.RFC3339), document.SourceMetadataPrecisionSecond)
				ids = append(ids, id)
				if random.IntN(4) != 0 {
					angle := float64(random.IntN(9)-4) * .14
					index.publish(t, id, []float64{math.Cos(angle), math.Sin(angle), 0, 0, 0, 0, 0, 0})
				}
			}
			index.generation(t, "mixed-vector-frames")
			coverage, err := f.s.PhotoAssetForNode(t.Context(), f.node.ID)
			require.NoError(t, err)
			ids = append(ids, coverage.ID)
			groups, unavailable, _, incomplete, err := f.s.PhotoStackGroups(t.Context(), ids, false, "")
			require.NoError(t, err)
			require.Empty(t, unavailable)
			require.False(t, incomplete)
			require.NotEmpty(t, groups)
			for _, direction := range []string{"asc", "desc"} {
				for _, size := range []int{1, 3, 7, 50} {
					t.Run(fmt.Sprintf("%s/%d", direction, size), func(t *testing.T) {
						request := captureRequest(t, direction)
						request.PageSize = size
						items, stacks := stackPages(t, f.s, request)
						want := slices.Clone(groups)
						if direction == "desc" {
							slices.Reverse(want)
						}
						require.Equal(t, want, stacks)
						require.ElementsMatch(t, ids, slices.Concat(items...), "every photo appears once")
					})
				}
			}
		})
	}
}

func TestPhotoStackIndexedOversizedRunKeepsIndividualPhotos(t *testing.T) {
	t.Parallel()
	f, ids := indexedPhotoRun(t, MaxPhotoStackFrames+2, 1)
	groups, unavailable, _, incomplete, err := f.s.PhotoStackGroups(t.Context(), ids, false, "")
	require.NoError(t, err)
	require.Empty(t, unavailable)
	require.False(t, incomplete)
	require.Empty(t, groups)
	coverage, err := f.s.PhotoAssetForNode(t.Context(), f.node.ID)
	require.NoError(t, err)
	for _, direction := range []string{"asc", "desc"} {
		request := captureRequest(t, direction)
		request.PageSize = 250
		items, stacks := stackPages(t, f.s, request)
		require.Empty(t, stacks)
		require.ElementsMatch(t, append(slices.Clone(ids), coverage.ID), slices.Concat(items...))
	}
}

func TestReviewNoModelBurstCrossesClockHour(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	run, err := s.BeginIngest(t.Context(), "cli", "synthetic clock boundary")
	require.NoError(t, err)
	var ids []string
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 502 {
		switch i {
		case 179:
			stamp = stamp.Add(19 * time.Second)
		case 180:
			stamp = stamp.Add(-19 * time.Second)
		}
		ids = append(ids, stackPhoto(t, s, run, fmt.Sprintf("/clock/%d.jpg", i), stamp.Format(time.RFC3339), document.SourceMetadataPrecisionSecond))
		stamp = stamp.Add(20 * time.Second)
	}
	want := [][]string{ids[179:181]}
	t.Run("Cull", func(t *testing.T) {
		groups, unavailable, _, _, err := s.PhotoStackGroups(t.Context(), ids, false, "")
		require.NoError(t, err)
		require.Empty(t, unavailable)
		require.Equal(t, want, groups)
	})
	for _, direction := range []string{"asc", "desc"} {
		t.Run(direction, func(t *testing.T) {
			request := captureRequest(t, direction)
			request.PageSize = 250
			_, stacks := stackPages(t, s, request)
			require.Equal(t, want, stacks)
		})
	}
}

func TestReviewOversizedNoModelBurstStaysIndividual(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	run, err := s.BeginIngest(t.Context(), "cli", "synthetic oversized clock burst")
	require.NoError(t, err)
	var ids []string
	stamp := time.Date(2026, 1, 1, 0, 55, 0, 0, time.UTC)
	for i := range 502 {
		ids = append(ids, stackPhoto(t, s, run, fmt.Sprintf("/oversized-clock/%d.jpg", i), stamp.Add(time.Duration(i)*time.Second).Format(time.RFC3339), document.SourceMetadataPrecisionSecond))
	}
	t.Run("Cull", func(t *testing.T) {
		groups, unavailable, _, _, err := s.PhotoStackGroups(t.Context(), ids, false, "")
		require.NoError(t, err)
		require.Empty(t, unavailable)
		require.Empty(t, groups)
	})
	for _, direction := range []string{"asc", "desc"} {
		t.Run(direction, func(t *testing.T) {
			request := captureRequest(t, direction)
			request.PageSize = 250
			items, stacks := stackPages(t, s, request)
			require.Empty(t, stacks)
			require.ElementsMatch(t, ids, slices.Concat(items...))
		})
	}
}

func TestPhotoStackIndexedBurstCrossesClockHour(t *testing.T) {
	t.Parallel()
	f := newImageEmbeddingCatalogFixture(t)
	run, err := f.s.BeginIngest(t.Context(), "cli", "synthetic indexed clock boundary")
	require.NoError(t, err)
	index := newCursorIndexFixture(f)
	var ids []string
	for i := range 2 {
		stamp := time.Date(2026, 1, 1, 12, 59, 55+i*10, 0, time.UTC)
		id := stackPhoto(t, f.s, run, fmt.Sprintf("/indexed-clock/%d.jpg", i), stamp.Format(time.RFC3339), document.SourceMetadataPrecisionSecond)
		ids = append(ids, id)
		index.publish(t, id, []float64{1, 0, 0, 0, 0, 0, 0, 0})
	}
	index.generation(t, "indexed-clock")
	groups, unavailable, _, _, err := f.s.PhotoStackGroups(t.Context(), ids, false, "")
	require.NoError(t, err)
	require.Empty(t, unavailable)
	require.Equal(t, [][]string{ids}, groups)
	for _, direction := range []string{"asc", "desc"} {
		for _, size := range []int{1, 250} {
			t.Run(fmt.Sprintf("%s/%d", direction, size), func(t *testing.T) {
				request := captureRequest(t, direction)
				request.PageSize = size
				_, stacks := stackPages(t, f.s, request)
				want := slices.Clone(groups)
				if direction == "desc" {
					slices.Reverse(want)
				}
				require.Equal(t, want, stacks)
			})
		}
	}
}
