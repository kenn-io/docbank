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
