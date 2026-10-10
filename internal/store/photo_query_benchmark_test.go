package store

import (
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
)

func BenchmarkPhotoBrowse10K(b *testing.B) { benchmarkPhotoBrowse(b, 10000) }

func BenchmarkPhotoBrowse100K(b *testing.B) { benchmarkPhotoBrowse(b, 100000) }

func benchmarkPhotoBrowse(b *testing.B, assetCount int) {
	b.Helper()
	b.StopTimer()
	s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, s.Close()) })
	ctx := b.Context()
	_, err = s.db.ExecContext(ctx, `PRAGMA synchronous=OFF`)
	require.NoError(b, err)
	for i := range assetCount {
		node, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("capture-%05d.jpg", i), browseHash(fmt.Sprintf("benchmark-source-%d", i)), 20, "image/jpeg")
		require.NoError(b, err)
		fields := []document.SourceMetadataFieldV1{photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("Synthetic Camera"))}
		if i < assetCount*9/10 {
			stamp := time.Date(2024, 1, 1, 3, 4, 5, 0, time.UTC).Add(time.Duration(i/2) * time.Hour).Format("2006-01-02T15:04:05")
			precision, zone, offset := document.SourceMetadataPrecisionSecond, document.SourceMetadataTimezoneOmitted, ""
			switch (i / 2) % 4 {
			case 1:
				stamp += "Z"
				zone = document.SourceMetadataTimezoneUTC
			case 2:
				stamp += "+02:00"
				zone, offset = document.SourceMetadataTimezoneOffset, "+02:00"
			case 3:
				stamp += ".123456789"
				precision = document.SourceMetadataPrecisionFraction
			}
			fields = append(fields, photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp(stamp, stamp, precision, zone, offset)))
		}
		canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: fields})
		require.NoError(b, err)
		_, err = s.PublishSourceMetadata(ctx, node.BlobHash, browseHash("benchmark-metadata"), canonical)
		require.NoError(b, err)
	}
	_, err = s.db.ExecContext(ctx, `PRAGMA synchronous=FULL`)
	require.NoError(b, err)
	var assets, heads, projected, captured int
	require.NoError(b, s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM photo_assets), (SELECT count(*) FROM source_metadata_heads), (SELECT count(*) FROM photo_technical_metadata), (SELECT count(*) FROM photo_technical_metadata WHERE capture_time IS NOT NULL)`).Scan(&assets, &heads, &projected, &captured))
	require.Equal(b, assetCount, assets)
	require.Equal(b, assetCount, heads)
	require.Equal(b, assetCount, projected)
	require.Equal(b, assetCount*9/10, captured)
	b.Logf("go=%s driver=%s assets=%d active_heads=%d projected=%d captured=%d missing=%d page_size=50 capture_mix=omitted_UTC_offset_fraction repeated_keys=pairs later_boundary=after_first_50", runtime.Version(), DefaultSQLiteDriver().Name(), assets, heads, projected, captured, assetCount-captured)
	recipes := map[string]string{}
	for size, edge := range map[string]int{"grid": 512, "fit": 2560, "large": 4096} {
		recipe := visualPreviewRecipe()
		recipe.MaxEdgePixels = edge
		_, fingerprint, err := document.MarshalVisualPreviewRecipeV1(recipe)
		require.NoError(b, err)
		recipes[size] = fingerprint
	}
	for _, tc := range []struct{ name, raw string }{
		{"ranked", `{"text":"Synthetic","sort":{"field":"relevance","direction":"desc"}}`},
		{"capture_asc", `{"sort":{"field":"capture_time","direction":"asc"}}`},
		{"capture_desc", `{"sort":{"field":"capture_time","direction":"desc"}}`},
		{"import", `{"sort":{"field":"import_time","direction":"desc"}}`},
		{"duplicates", `{"sort":{"field":"import_time","direction":"desc"},"filters":{"collapse_duplicates":true}}`},
		{"capture_range", `{"sort":{"field":"capture_time","direction":"asc"},"filters":{"capture_after":"2024-03-01","capture_before":"2024-04-01"}}`},
	} {
		value, err := query.Parse([]byte(tc.raw))
		require.NoError(b, err)
		request := PhotoBrowseRequest{Query: value, PageSize: 50, Recipes: recipes}
		if tc.name == "ranked" {
			b.Run("ranked/counts", func(b *testing.B) {
				counts := request
				counts.Query.Sort = query.Sort{Field: "capture_time", Direction: "desc"}
				counts.PageSize = 1
				counts.Facets = []string{"camera", "lens", "year", "location", "set"}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					page, err := s.ListPhotoAssets(ctx, counts, nil)
					require.NoError(b, err)
					require.Len(b, page.Facets, 5)
					var available int
					for _, facet := range page.Facets {
						if facet.Available {
							available++
						}
					}
					b.ReportMetric(float64(available), "available-facets")
				}
			})
			b.Run("ranked/first", func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					page, err := s.ListPhotoAssets(ctx, request, nil)
					require.NoError(b, err)
					require.Len(b, page.Items, 50)
					require.Equal(b, int64(assetCount), page.Total)
					require.Empty(b, page.Facets)
					require.Nil(b, page.Next)
				}
			})
			continue
		}
		first, err := s.ListPhotoAssets(ctx, request, nil)
		require.NoError(b, err)
		require.Len(b, first.Items, 50)
		require.NotNil(b, first.Next)
		if tc.name != "capture_range" {
			require.Equal(b, int64(assetCount), first.Total)
		} else {
			require.Equal(b, int64(1488), first.Total)
		}
		for _, page := range []struct {
			name     string
			boundary *PhotoBrowsePosition
		}{{"first", nil}, {"later", first.Next}} {
			check, err := s.ListPhotoAssets(ctx, request, page.boundary)
			require.NoError(b, err)
			require.Len(b, check.Items, 50)
			require.Len(b, check.Items[0].Previews, 3)
			b.Run(tc.name+"/"+page.name, func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if _, err := s.ListPhotoAssets(ctx, request, page.boundary); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
