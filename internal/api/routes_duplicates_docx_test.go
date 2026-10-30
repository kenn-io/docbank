package api_test

import (
	"archive/zip"
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestDOCXDuplicatesHTTPFromOriginals(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	originals := map[int64][]byte{}
	var targets []store.SourceMetadataTarget
	for _, year := range []int{2025, 2026} {
		var b bytes.Buffer
		w := zip.NewWriter(&b)
		for _, part := range []struct{ name, body string }{
			{"[Content_Types].xml", `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`},
			{"word/document.xml", `<document>Synthetic sample</document>`},
		} {
			f, err := w.CreateHeader(&zip.FileHeader{Name: part.name, Modified: time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)})
			require.NoError(t, err)
			_, err = f.Write([]byte(part.body))
			require.NoError(t, err)
		}
		require.NoError(t, w.Close())
		hash, size, err := s.Blobs.Write(bytes.NewReader(b.Bytes()))
		require.NoError(t, err)
		node, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("sample-%d.docx", year), hash, size,
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document")
		require.NoError(t, err)
		originals[node.ID] = b.Bytes()
		targets = append(targets, store.SourceMetadataTarget{SourceSHA256: hash, Size: size})
	}
	resp, body := get(t, ts, "/api/v1/duplicates/docx", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page struct {
		Items []struct {
			Fingerprint string     `json:"fingerprint"`
			References  []api.Node `json:"references"`
		} `json:"items"`
		Total                     int `json:"total"`
		UnfingerprintedReferences int `json:"unfingerprinted_references"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Equal(t, 2, page.UnfingerprintedReferences)
	require.Empty(t, page.Items)
	count, err := processing.BackfillSourceMetadataTargets(ctx, s.Store, s.Blobs, t.TempDir(), targets)
	require.NoError(t, err)
	require.Equal(t, 2, count)
	resp, body = get(t, ts, "/api/v1/duplicates/docx", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Zero(t, page.UnfingerprintedReferences)
	require.Equal(t, 1, page.Total)
	require.Len(t, page.Items, 1)
	require.Len(t, page.Items[0].References, 2)
	refs := page.Items[0].References
	require.NotEqual(t, refs[0].BlobHash, refs[1].BlobHash)
	for _, ref := range refs {
		require.NotEmpty(t, ref.Path)
		resp, body = get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/content", ref.ID), nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		require.Equal(t, originals[ref.ID], []byte(body))
	}
	resp, body = get(t, ts, "/api/v1/duplicates", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var exact api.DuplicatePage
	require.NoError(t, json.Unmarshal([]byte(body), &exact))
	require.Empty(t, exact.Items)
}
