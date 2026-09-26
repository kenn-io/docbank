package store_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestProductionSourcePDFRouteServesExactDraftMember(t *testing.T) {
	fixture := newProductionPreviewHTTPFixture(t, true)
	members, _, err := fixture.vault.ProductionMembers(t.Context(), fixture.setID, fixture.revision, "", 10)
	require.NoError(t, err)
	require.Len(t, members, 1)
	member := members[0]
	require.NotEqual(t, member.SourceSHA256, member.PDFSHA256, "the selected original is not the editor PDF")
	path := "/api/v1/productions/sets/" + fixture.setID + "/revisions/" +
		strconv.FormatInt(fixture.revision, 10) + "/members/" + fixture.memberID + "/pdf"
	get := func(path, etag string) *http.Response {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fixture.ts.URL+path, nil)
		require.NoError(t, err)
		request.Header.Set("If-Match", etag)
		response, err := fixture.ts.Client().Do(request)
		require.NoError(t, err)
		return response
	}
	response := get(path, strconv.FormatInt(fixture.etag, 10))
	bytes, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "application/pdf", response.Header.Get("Content-Type"))
	require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
	require.Equal(t, member.PDFSHA256, response.Header.Get(api.BlobHashHeader))
	require.Equal(t, strconv.FormatInt(member.PDFSize, 10), response.Header.Get(api.BlobSizeHeader))
	require.NotEmpty(t, response.Header.Get("Content-Digest"))
	require.Equal(t, member.PDFSize, int64(len(bytes)))
	require.Contains(t, string(bytes[:min(len(bytes), 8)]), "%PDF")
	digest := sha256.Sum256(bytes)
	require.Equal(t, member.PDFSHA256, hex.EncodeToString(digest[:]))

	for _, tc := range []struct {
		path, etag string
		status     int
	}{
		{path, strconv.FormatInt(fixture.etag-1, 10), http.StatusConflict},
		{path, "", http.StatusPreconditionRequired},
		{"/api/v1/productions/sets/" + fixture.setID + "/revisions/1/members/89000000-0000-4000-8000-000000000099/pdf", strconv.FormatInt(fixture.etag, 10), http.StatusNotFound},
	} {
		response := get(tc.path, tc.etag)
		require.Equal(t, tc.status, response.StatusCode)
		require.NoError(t, response.Body.Close())
	}
	withoutKey, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fixture.ts.URL+path, nil)
	require.NoError(t, err)
	unauthorized, err := http.DefaultClient.Do(withoutKey)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, unauthorized.StatusCode)
	require.NoError(t, unauthorized.Body.Close())
}

func TestProductionSourcePDFRouteRejectsMissingBytes(t *testing.T) {
	fixture := newProductionPreviewHTTPFixture(t, false)
	path := "/api/v1/productions/sets/" + fixture.setID + "/revisions/" +
		strconv.FormatInt(fixture.revision, 10) + "/members/" + fixture.memberID + "/pdf"
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fixture.ts.URL+path, nil)
	require.NoError(t, err)
	request.Header.Set("If-Match", strconv.FormatInt(fixture.etag, 10))
	response, err := fixture.ts.Client().Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, response.StatusCode)
	require.NoError(t, response.Body.Close())
}
