package store

import (
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestPhotoStackImageStacksSpanAtMostAMinute(t *testing.T) {
	t.Parallel()
	for _, count := range []int{3, 4, 5, 180} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			f := newImageEmbeddingCatalogFixture(t)
			run, err := f.s.BeginIngest(t.Context(), "cli", "synthetic minute boundary")
			require.NoError(t, err)
			index := newCursorIndexFixture(f)
			ids := make([]string, count)
			for i := range ids {
				stamp := time.Date(2026, 1, 1, 12, 0, i*20, 0, time.UTC).Format(time.RFC3339)
				ids[i] = stackPhoto(t, f.s, run, fmt.Sprintf("/minute/%d.jpg", i), stamp, document.SourceMetadataPrecisionSecond)
				index.publish(t, ids[i], []float64{1, 0, 0, 0, 0, 0, 0, 0})
			}
			index.generation(t, "minute-span")
			var want [][]string
			if count <= 4 {
				want = [][]string{ids}
			}
			assertPhotoStackSpanPages(t, f.s, ids, want)
		})
	}
}

func TestPhotoStackFailedImageChainKeepsBurstStacks(t *testing.T) {
	t.Parallel()
	f := newImageEmbeddingCatalogFixture(t)
	run, err := f.s.BeginIngest(t.Context(), "cli", "synthetic burst and time lapse")
	require.NoError(t, err)
	index := newCursorIndexFixture(f)
	var ids []string
	for i, second := range []int{0, 1, 2, 22, 42, 62, 82, 102, 122} {
		stamp := time.Date(2026, 1, 1, 12, 0, second, 0, time.UTC).Format(time.RFC3339)
		id := stackPhoto(t, f.s, run, fmt.Sprintf("/burst-lapse/%d.jpg", i), stamp, document.SourceMetadataPrecisionSecond)
		ids = append(ids, id)
		index.publish(t, id, []float64{1, 0, 0, 0, 0, 0, 0, 0})
	}
	index.generation(t, "burst-lapse")
	assertPhotoStackSpanPages(t, f.s, ids, [][]string{ids[:3]})
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
