package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

func TestPhotoStackCursorSurvivesIndexPublication(t *testing.T) {
	t.Parallel()
	type direction struct {
		name, last, next string
		size             int
	}
	type scenario struct {
		burst      []string
		indexed    []string // photos published before the cursors are taken
		published  []string // photos published in the generation the cursors resume across
		directions []direction
	}
	for _, tc := range []struct {
		name  string
		setup func(photo func(source, stamp string) string) scenario
	}{
		{"unrelated publication", func(photo func(source, stamp string) string) scenario {
			// A burst in /A, a neighbour in /B on each side of it, and a photo hours away.
			before := photo("/B/before.jpg", "2024-06-10T11:59:58Z")
			burst := []string{photo("/A/one.jpg", "2024-06-10T12:00:00Z"), photo("/A/two.jpg", "2024-06-10T12:00:01Z")}
			after := photo("/B/after.jpg", "2024-06-10T12:00:03Z")
			unrelated := photo("/C/far.jpg", "2024-06-10T18:00:00Z")
			return scenario{burst: burst, indexed: burst, published: []string{unrelated}, directions: []direction{{"asc", "", after, 2}, {"desc", "", before, 3}}}
		}},
		{"first index", func(photo func(source, stamp string) string) scenario {
			// An unindexed burst sits between two pairs of neighbours that are 29s and
			// 30s away: outside the 2s burst gap, inside the 60s indexed gap.
			w := photo("/B/w.jpg", "2024-06-10T11:59:00Z")
			x := photo("/B/x.jpg", "2024-06-10T11:59:30Z")
			burst := []string{photo("/A/one.jpg", "2024-06-10T12:00:00Z"), photo("/A/two.jpg", "2024-06-10T12:00:01Z")}
			y := photo("/B/y.jpg", "2024-06-10T12:00:30Z")
			z := photo("/B/z.jpg", "2024-06-10T12:01:00Z")
			far := photo("/C/far.jpg", "2024-06-10T18:00:00Z")
			// The first index covers the burst itself and the far photo: no stack membership changes.
			return scenario{burst: burst, published: append(append([]string{}, burst...), far), directions: []direction{{"asc", y, z, 5}, {"desc", x, w, 6}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newImageEmbeddingCatalogFixture(t)
			s := f.s
			run, err := s.BeginIngest(t.Context(), "cli", "synthetic cursor window")
			require.NoError(t, err)
			sc := tc.setup(func(source, stamp string) string {
				return stackPhoto(t, s, run, source, stamp, document.SourceMetadataPrecisionSecond)
			})
			index := newCursorIndexFixture(f)
			if len(sc.indexed) > 0 {
				for _, id := range sc.indexed {
					index.publish(t, id, []float64{1, 0, 0, 0, 0, 0, 0, 0})
				}
				index.generation(t, "cursor-generation-before")
			}
			cursors := make([]*PhotoBrowsePosition, len(sc.directions))
			for i, d := range sc.directions {
				request := captureRequest(t, d.name)
				request.PageSize = d.size
				first, err := s.ListPhotoAssets(t.Context(), request, nil)
				require.NoError(t, err)
				if d.last != "" {
					require.Contains(t, rowIDs(first.Items), d.last, d.name)
				}
				require.NotContains(t, rowIDs(first.Items), d.next, d.name)
				require.Equal(t, [][]string{sc.burst}, stackAssetIDs(first.Stacks), d.name)
				require.NotNil(t, first.Next)
				cursors[i] = first.Next
			}
			for _, id := range sc.published {
				index.publish(t, id, []float64{0, 1, 0, 0, 0, 0, 0, 0})
			}
			index.generation(t, "cursor-generation-after")
			for i, d := range sc.directions {
				t.Run(d.name, func(t *testing.T) {
					request := captureRequest(t, d.name)
					request.PageSize = d.size
					next, err := s.ListPhotoAssets(t.Context(), request, cursors[i])
					require.NoError(t, err, "an index publication that changes no stack must preserve its cursor")
					require.Contains(t, rowIDs(next.Items), d.next)
				})
			}
		})
	}
}

func stackAssetIDs(stacks []PhotoStack) [][]string {
	ids := make([][]string, len(stacks))
	for i, stack := range stacks {
		ids[i] = stack.AssetIDs
	}
	return ids
}

// cursorIndexFixture accumulates published image vectors and builds index
// generations over them for the cursor tests.
type cursorIndexFixture struct {
	f         imageEmbeddingCatalogFixture
	vectors   []document.VectorSetV1
	checksums []string
	space     string
}

func newCursorIndexFixture(f imageEmbeddingCatalogFixture) *cursorIndexFixture {
	return &cursorIndexFixture{f: f}
}

func (x *cursorIndexFixture) publish(t *testing.T, assetID string, values []float64) {
	t.Helper()
	asset, err := x.f.s.PhotoAssetByID(t.Context(), assetID)
	require.NoError(t, err)
	node, err := x.f.s.NodeByID(t.Context(), asset.Files[0].NodeID)
	require.NoError(t, err)
	record, vector := x.f.forNode(t, node).publish(t, values)
	x.add(record, vector)
}

// add accumulates a vector set the caller published itself.
func (x *cursorIndexFixture) add(record EmbeddingSetRecord, vector document.VectorSetV1) {
	x.vectors, x.checksums, x.space = append(x.vectors, vector), append(x.checksums, record.VectorSet.ID), record.VectorSpace.ID
}

func (x *cursorIndexFixture) generation(t *testing.T, label string) string {
	t.Helper()
	manifest, err := vectorindex.NewManifest(x.checksums)
	require.NoError(t, err)
	index, err := vectorindex.BuildGeneration(manifest, x.vectors, vectorindex.Options{})
	require.NoError(t, err)
	source, err := x.f.s.CaptureVectorIndexSource(t.Context(), x.space)
	require.NoError(t, err)
	id := browseHash(label)
	require.NoError(t, putVectorIndexGenerationForTest(t, x.f.s, VectorIndexGenerationRecord{ID: id, VectorSpaceID: x.space, SourceManifestChecksum: source.ManifestChecksum, IndexManifestChecksum: index.Metadata().Manifest.Checksum, Bytes: index.Bytes(), RowCount: len(x.vectors), BuiltAt: embeddingCatalogTime}))
	return id
}
