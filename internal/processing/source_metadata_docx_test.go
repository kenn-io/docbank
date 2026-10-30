package processing

import (
	"archive/zip"
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestSourceMetadataDOCXFingerprint(t *testing.T) {
	t.Parallel()
	var fingerprints []string
	for _, year := range []int{2025, 2026} {
		var b bytes.Buffer
		w := zip.NewWriter(&b)
		for _, part := range []struct{ name, body string }{
			{"[Content_Types].xml", `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`},
			{"word/document.xml", `<document>Synthetic text</document>`},
		} {
			f, err := w.CreateHeader(&zip.FileHeader{Name: part.name, Modified: time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)})
			require.NoError(t, err)
			_, err = f.Write([]byte(part.body))
			require.NoError(t, err)
		}
		require.NoError(t, w.Close())
		metadata, err := ExtractSourceMetadata(t.Context(), sourceMetadataTestSpool(t), b.Bytes())
		require.NoError(t, err)
		encoded, _, err := document.MarshalSourceMetadataV1(metadata)
		require.NoError(t, err)
		decoded, _, err := document.DecodeSourceMetadataV1(encoded)
		require.NoError(t, err)
		var fingerprint string
		for _, field := range decoded.Fields {
			if field.Key == "office.package.docx_sha256_v1" {
				fingerprint = *field.Value.String
			}
		}
		require.Len(t, fingerprint, 64)
		fingerprints = append(fingerprints, fingerprint)
	}
	require.Equal(t, fingerprints[0], fingerprints[1])
}
