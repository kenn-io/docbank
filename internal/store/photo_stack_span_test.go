package store

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestPhotoStackPagingMatchesGroups(t *testing.T) {
	t.Parallel()
	type testCase struct {
		name      string
		seconds   []int
		vectors   [][]float64
		groups    [][]int
		unindexed bool
	}
	var cases []testCase
	for _, count := range []int{3, 4, 5, 180} {
		seconds := make([]int, count)
		for i := range seconds {
			seconds[i] = i * 20
		}
		var groups [][]int
		if count <= 4 {
			group := make([]int, count)
			for i := range group {
				group[i] = i
			}
			groups = [][]int{group}
		}
		cases = append(cases, testCase{name: fmt.Sprintf("minute boundary/%d", count), seconds: seconds, groups: groups})
	}
	cases = append(cases,
		testCase{
			name:      "unindexed images",
			seconds:   []int{0, 60, 80, 141},
			unindexed: true,
		},
		testCase{
			name:    "above cutoff with a dissimilar middle frame",
			seconds: []int{0, 60, 80, 141},
			vectors: [][]float64{{1, 0, 0, 0, 0, 0, 0, 0}, {.99, .14106736, 0, 0, 0, 0, 0, 0}, {0, 1, 0, 0, 0, 0, 0, 0}, {1, 0, 0, 0, 0, 0, 0, 0}},
			groups:  [][]int{{0, 1}},
		},
		testCase{
			name:    "identical images outside window",
			seconds: []int{0, 141},
			vectors: [][]float64{{1, 0, 0, 0, 0, 0, 0, 0}, {1, 0, 0, 0, 0, 0, 0, 0}},
		},
		testCase{
			name:    "nontransitive similarity",
			seconds: []int{0, 20, 40},
			vectors: [][]float64{{1, 0, 0, 0, 0, 0, 0, 0}, {.96, .28, 0, 0, 0, 0, 0, 0}, {.96, -.28, 0, 0, 0, 0, 0, 0}},
			groups:  [][]int{{0, 1}},
		},
		testCase{
			name:    "burst then time lapse",
			seconds: []int{0, 1, 2, 22, 42, 62, 82, 102, 122},
			groups:  [][]int{{0, 1, 2}},
		},
	)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newImageEmbeddingCatalogFixture(t)
			run, err := f.s.BeginIngest(t.Context(), "cli", "synthetic indexed photo run")
			require.NoError(t, err)
			index := newCursorIndexFixture(f)
			ids := make([]string, len(tc.seconds))
			for i, second := range tc.seconds {
				stamp := time.Date(2026, 1, 1, 12, 0, second, 0, time.UTC).Format(time.RFC3339)
				ids[i] = stackPhoto(t, f.s, run, fmt.Sprintf("/indexed/%d.jpg", i), stamp, document.SourceMetadataPrecisionSecond)
				vector := []float64{1, float64(i%7) * .01, 0, 0, 0, 0, 0, 0}
				if len(tc.vectors) > 0 {
					vector = tc.vectors[i]
				}
				if !tc.unindexed {
					index.publish(t, ids[i], vector)
				}
			}
			if !tc.unindexed {
				index.generation(t, "indexed-photo-run")
			}
			var want [][]string
			if len(tc.groups) > 0 {
				want = make([][]string, len(tc.groups))
			}
			for i, group := range tc.groups {
				for _, member := range group {
					want[i] = append(want[i], ids[member])
				}
			}
			assertPhotoStackSpanPages(t, f.s, ids, want)
		})
	}
	t.Run("random mixed vectors", func(t *testing.T) {
		f := newImageEmbeddingCatalogFixture(t)
		run, err := f.s.BeginIngest(t.Context(), "cli", "synthetic mixed-vector frames")
		require.NoError(t, err)
		index := newCursorIndexFixture(f)
		random := rand.New(rand.NewPCG(1030, 839)) //nolint:gosec // Fixed seeds reproduce synthetic grouping cases.
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
	t.Run("interleaved folders with equal timestamps across page boundaries", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		run, err := s.BeginIngest(t.Context(), "cli", "synthetic interleaved bursts")
		require.NoError(t, err)
		steps := []int{0, 1, 3, 2, 0, 1, 1, 3, 3, 2, 1, 0, 2, 3, 1, 2, 0, 3, 1, 1}
		base := time.Date(2024, 6, 10, 12, 0, 0, 0, time.UTC)
		var ids []string
		for folder, offset := range []int{0, 1, 2} {
			stamp := base.Add(time.Duration(offset) * time.Second)
			for index := range steps {
				stamp = stamp.Add(time.Duration(steps[(index+folder*7)%len(steps)]) * time.Second)
				ids = append(ids, stackPhoto(t, s, run, fmt.Sprintf("/roll-%d/frame-%02d.jpg", folder, index),
					stamp.Format(time.RFC3339), document.SourceMetadataPrecisionSecond))
			}
		}
		groups, _, _, incomplete, err := s.PhotoStackGroups(t.Context(), ids, false, "")
		require.NoError(t, err)
		require.False(t, incomplete)
		want := make([][]string, len(groups))
		for index, group := range groups {
			want[index] = slices.Sorted(slices.Values(group))
		}
		require.NotEmpty(t, want)
		for _, size := range []int{1, 2, 3, 7, 50} {
			for _, direction := range []string{"asc", "desc"} {
				request := captureRequest(t, direction)
				request.PageSize = size
				items, stacks := stackPages(t, s, request)
				got := make([][]string, len(stacks))
				for index, stack := range stacks {
					got[index] = slices.Sorted(slices.Values(stack))
				}
				require.ElementsMatch(t, want, got, "size %d %s", size, direction)
				require.ElementsMatch(t, ids, slices.Concat(items...), "size %d %s: every photo appears once", size, direction)
			}
		}
	})
}

func assertPhotoStackSpanPages(t *testing.T, s *Store, ids []string, want [][]string) {
	t.Helper()
	groups, unavailable, _, incomplete, err := s.PhotoStackGroups(t.Context(), ids, false, "")
	require.NoError(t, err)
	require.Empty(t, unavailable)
	require.False(t, incomplete)
	require.Len(t, groups, len(want))
	if len(want) > 0 {
		require.Equal(t, want, groups)
	}
	for _, direction := range []string{"asc", "desc"} {
		for _, size := range []int{1, 50} {
			t.Run(fmt.Sprintf("%s/%d", direction, size), func(t *testing.T) {
				request := captureRequest(t, direction)
				request.PageSize = size
				_, stacks := stackPages(t, s, request)
				expected := slices.Clone(want)
				if direction == "desc" {
					slices.Reverse(expected)
				}
				require.Equal(t, expected, stacks)
			})
		}
	}
}
