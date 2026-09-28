package api_test

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"golang.org/x/image/tiff"
)

func TestPackageExportPreservesPageImageFormats(t *testing.T) {
	for _, format := range []struct{ name, extension string }{{"tiff", ".tif"}, {"jpeg", ".jpg"}, {"png", ".png"}} {
		t.Run(format.name, func(t *testing.T) {
			img := image.NewRGBA(image.Rect(0, 0, 3, 3))
			img.Set(1, 1, color.RGBA{R: 255, A: 255})
			var original bytes.Buffer
			switch format.name {
			case "tiff":
				require.NoError(t, tiff.Encode(&original, img, nil))
			case "jpeg":
				require.NoError(t, jpeg.Encode(&original, img, nil))
			case "png":
				require.NoError(t, png.Encode(&original, img))
			}
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, "VOL001"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "data.dat"), []byte("DOCID\nSOURCE\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "data.opt"), []byte("SOURCE,VOL001,page"+format.extension+",Y,,,1\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "page"+format.extension), original.Bytes(), 0o600))
			source, catalog := newPackageTestServer(t)
			pkg, _ := importExportFixture(t, source, catalog, root, "dat-concordance-v1", nil)
			for _, profile := range []string{"export-dat-opt-images-v1", "export-dat-lfp-images-v1"} {
				t.Run(profile, func(t *testing.T) {
					var archive bytes.Buffer
					_, err := processing.WriteLoadFileExport(t.Context(), catalog.Store, catalog.Blobs,
						processing.LoadFileExportRequest{SnapshotID: pkg.SnapshotID, ProfileID: profile}, &archive)
					if profile == "export-dat-lfp-images-v1" && format.name != "tiff" {
						require.ErrorIs(t, err, loadfile.ErrUnrepresentable)
						require.Empty(t, archive.Bytes(), "reject unsupported LFP images before writing the package")
						return
					}
					require.NoError(t, err)
					verified, err := processing.VerifyLoadFileExport(t.Context(), bytes.NewReader(archive.Bytes()), int64(archive.Len()))
					require.NoError(t, err)
					extracted, err := loadfile.ExtractZIP(t.Context(), bytes.NewReader(archive.Bytes()), int64(archive.Len()))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, loadfile.RemoveExtractedZIP(extracted)) })
					outputName := "DOC000001-000001" + format.extension
					output, err := os.ReadFile(filepath.Join(extracted, "VOL001", "IMAGES", outputName))
					require.NoError(t, err)
					require.Equal(t, original.Bytes(), output)
					_, decodedFormat, err := image.DecodeConfig(bytes.NewReader(output))
					require.NoError(t, err)
					require.Equal(t, format.name, decodedFormat)
					pageMap, err := os.ReadFile(filepath.Join(extracted, filepath.FromSlash(verified.Receipt.PageMap)))
					require.NoError(t, err)
					require.Contains(t, string(pageMap), outputName)
					mapping, err := canonical.Marshal(verified.Receipt.Mapping)
					require.NoError(t, err)
					fresh, freshCatalog := newPackageTestServer(t)
					_, members := importExportFixture(t, fresh, freshCatalog, extracted, verified.Receipt.Profile.ID, mapping)
					require.Len(t, members, 1)
					page := slices.IndexFunc(members[0].Representations, func(rep store.CollectionSnapshotRepresentation) bool { return rep.Role == "page_image" })
					require.NotEqual(t, -1, page)
					require.Equal(t, "image/"+format.name, members[0].Representations[page].MediaType)
				})
			}
		})
	}
}
