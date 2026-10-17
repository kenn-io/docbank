package store

import (
	"database/sql"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
)

const photoStackScaleAssets = 250_000

// The scale vaults spread frames over 25 folders, one frame per folder at a time.
const photoStackScaleFolders = 25

func photoStackScaleSource(i int) string {
	return fmt.Sprintf("/library/roll-%02d/frame-%06d.jpg", i%photoStackScaleFolders, i/photoStackScaleFolders)
}

// BenchmarkPhotoStackBrowse250K compares grouped capture-time pages with the
// plain keyset over 250,000 matches. Set DOCBANK_PHOTO_STACK_SCALE=1 to run it.
// The seeded vaults stay in the repository's ignored .superpowers directory,
// so each case can run in its own process and report its peak RSS without
// the seeding cost.
func BenchmarkPhotoStackBrowse250K(b *testing.B) {
	if os.Getenv("DOCBANK_PHOTO_STACK_SCALE") != "1" {
		b.Skip("set DOCBANK_PHOTO_STACK_SCALE=1 for the 250k burst measurements")
	}
	dir := filepath.Join("..", "..", ".superpowers", fmt.Sprintf("photo-stack-scale-v%d", currentStorageSchemaVersion))
	require.NoError(b, os.MkdirAll(dir, 0o700))
	// Bursts: 25 folders capture five frames one second apart at the same
	// instants, so every page interleaves stacks from many folders with tied
	// keys. Singles: the same folders capture one frame every 3 seconds, just
	// outside the 2-second gap, so no frame has a neighbor.
	for _, layout := range []struct {
		name  string
		frame func(i int) time.Duration
	}{
		{"bursts", func(i int) time.Duration { return time.Duration(i/5)*time.Minute + time.Duration(i%5)*time.Second }},
		{"singles", func(i int) time.Duration { return time.Duration(i) * 3 * time.Second }},
	} {
		b.Run(layout.name, func(b *testing.B) {
			s := photoStackScaleStore(b, filepath.Join(dir, strings.ReplaceAll(DefaultSQLiteDriver().Name(), "/", "-")+"-"+layout.name+".db"), layout.frame)
			for _, direction := range []string{"desc", "asc"} {
				value, err := query.Parse([]byte(`{"sort":{"field":"capture_time","direction":"` + direction + `"}}`))
				require.NoError(b, err)
				for _, stacks := range []bool{false, true} {
					mode := "plain"
					if stacks {
						mode = "stacks"
					}
					b.Run(direction+"-"+mode, func(b *testing.B) {
						request := PhotoBrowseRequest{Query: value, PageSize: 250, Stacks: stacks}
						first, err := s.ListPhotoAssets(b.Context(), request, nil)
						require.NoError(b, err)
						require.Equal(b, int64(photoStackScaleAssets), first.Total)
						require.NotNil(b, first.Next)
						if stacks && layout.name == "bursts" {
							require.Len(b, first.Stacks, 50)
						} else {
							require.Empty(b, first.Stacks)
						}
						for _, page := range []struct {
							name     string
							boundary *PhotoBrowsePosition
						}{{"first", nil}, {"later", first.Next}} {
							b.Run(page.name, func(b *testing.B) {
								b.ReportAllocs()
								for b.Loop() {
									result, err := s.ListPhotoAssets(b.Context(), request, page.boundary)
									if err != nil {
										b.Fatal(err)
									}
									if len(result.Items) != 250 {
										b.Fatalf("page has %d items, want 250", len(result.Items))
									}
								}
							})
						}
					})
				}
			}
		})
	}
}

// photoStackScaleStore opens path, seeding it first when it does not exist.
// Every frame is imported from its folder and has a second-precision EXIF time.
func photoStackScaleStore(b *testing.B, file string, frame func(int) time.Duration) *Store {
	b.Helper()
	_, statErr := os.Stat(file)
	s, err := Open(file)
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, s.Close()) })
	ctx := b.Context()
	if statErr != nil {
		require.ErrorIs(b, statErr, os.ErrNotExist)
		seedPhotoStackScale(b, s, frame)
	}
	var assets, keyed int
	require.NoError(b, s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM photo_assets),(SELECT count(*) FROM photo_timeline WHERE folder_key IS NOT NULL)`).Scan(&assets, &keyed))
	require.Equal(b, photoStackScaleAssets, assets)
	require.Equal(b, photoStackScaleAssets, keyed)
	return s
}

func seedPhotoStackScale(b *testing.B, s *Store, frame func(int) time.Duration) {
	b.Helper()
	ctx := b.Context()
	started := time.Now()
	_, err := s.db.ExecContext(ctx, `PRAGMA synchronous=OFF`)
	require.NoError(b, err)
	run, err := s.BeginIngest(ctx, "cli", "synthetic burst scale")
	require.NoError(b, err)
	base := time.Date(2024, 6, 10, 12, 0, 0, 0, time.UTC)
	for start := 0; start < photoStackScaleAssets; start += 1000 {
		require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
			for i := start; i < start+1000; i++ {
				source := photoStackScaleSource(i)
				if _, _, _, err := s.ingestFileTx(ctx, tx, run, s.RootID(), path.Base(path.Dir(source))+"-"+path.Base(source), browseHash(source), 20, "image/jpeg", source, "2024-01-01T00:00:00Z", ingestFileOptions{exact: true}); err != nil {
					return err
				}
			}
			return nil
		}))
	}
	b.Logf("imported elapsed=%s", time.Since(started))
	for i := range photoStackScaleAssets {
		stamp := base.Add(frame(i / photoStackScaleFolders)).Format(time.RFC3339)
		canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: []document.SourceMetadataFieldV1{
			photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp(stamp, stamp, document.SourceMetadataPrecisionSecond, document.SourceMetadataTimezoneUTC, "")),
		}})
		require.NoError(b, err)
		_, err = s.PublishSourceMetadata(ctx, browseHash(photoStackScaleSource(i)), browseHash("synthetic burst scale metadata"), canonical)
		require.NoError(b, err)
	}
	_, err = s.db.ExecContext(ctx, `PRAGMA synchronous=FULL`)
	require.NoError(b, err)
	b.Logf("seeded go=%s driver=%s assets=%d folders=%d elapsed=%s", runtime.Version(), DefaultSQLiteDriver().Name(), photoStackScaleAssets, photoStackScaleFolders, time.Since(started))
}
