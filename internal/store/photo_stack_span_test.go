package store

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestPhotoStackPagingMatchesGroups(t *testing.T) {
	t.Parallel()
	type testCase struct {
		name    string
		seconds []int
		vectors [][]float64
		groups  [][]int
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
		testCase{
			name:    "oversized run",
			seconds: make([]int, MaxPhotoStackFrames+2),
		},
	)
	for i := range cases[len(cases)-1].seconds {
		cases[len(cases)-1].seconds[i] = i
	}

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
				index.publish(t, ids[i], vector)
			}
			index.generation(t, "indexed-photo-run")
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
