// internal/api/routes_read_test.go
package api_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/api"
)

func TestStatByIDAndPath(t *testing.T) {
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	d, err := s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)

	resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d", d.ID), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var n api.Node
	require.NoError(t, json.Unmarshal([]byte(body), &n))
	assert.Equal(t, d.ID, n.ID)
	assert.Empty(t, n.BlobHash)
	assert.Equal(t, "/docs", n.Path)
	assert.Equal(t, fmt.Sprintf("%q", strconv.FormatInt(d.Revision, 10)), resp.Header.Get("ETag"))

	resp, body = get(t, ts, "/api/v1/path?path=%2Fdocs", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.Unmarshal([]byte(body), &n))
	assert.Equal(t, d.ID, n.ID)

	// Root stats fine; relative and missing paths are rejected.
	resp, _ = get(t, ts, "/api/v1/path?path=%2F", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp, body = get(t, ts, "/api/v1/path?path=docs", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, `"code":"validation"`)
	resp, body = get(t, ts, "/api/v1/path?path=%2Fnope", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Contains(t, body, `"code":"not_found"`)
}

func TestChildrenPagination(t *testing.T) {
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	for i := range 5 {
		_, err := s.Mkdir(ctx, s.RootID(), fmt.Sprintf("d%d", i))
		require.NoError(t, err)
	}
	resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/children?limit=2&offset=4", s.RootID()), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var page struct {
		Items []api.Node `json:"items"`
		Total int        `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Equal(t, 5, page.Total)
	assert.Len(t, page.Items, 1) // offset 4 of 5
}

func TestContentStreamsBlob(t *testing.T) {
	ts, s := newTestServer(t, nil)
	// Write a real blob through the test server's blob dir, then link it.
	n := createFileWithContent(t, ts, s, "/hello.txt", "hello world")
	resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/content", n.ID), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "hello world", body)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/plain")
	assert.Equal(t, n.BlobHash, resp.Header.Get(api.BlobHashHeader))
	assert.Equal(t, n.CurrentVersionID, resp.Header.Get(api.ContentVersionHeader))
	assert.Equal(t, strconv.FormatInt(n.Size, 10), resp.Header.Get(api.BlobSizeHeader))
	assert.Empty(t, resp.Header.Get("Content-Length"), "fixed length would suppress the digest trailer on HTTP/1.1")
	sum := sha256.Sum256([]byte("hello world"))
	assert.Equal(t, "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":",
		resp.Trailer.Get("Content-Digest"))
}

func TestFileNodeExposesBlobIdentity(t *testing.T) {
	ts, s := newTestServer(t, nil)
	n := createFileWithContent(t, ts, s, "/identity.txt", "identity")
	resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d", n.ID), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got api.Node
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	assert.Equal(t, n.BlobHash, got.BlobHash)
	assert.Equal(t, n.Size, got.Size)
	assert.Equal(t, n.CurrentVersionID, got.CurrentVersionID)
}

func TestContentVersionListMetadataAndPackedBytes(t *testing.T) {
	ts, s := newTestServer(t, nil)
	n := createFileWithContent(t, ts, s, "/versioned.txt", "stable version bytes")
	require.NotEmpty(t, n.CurrentVersionID)

	resp, body := get(t, ts,
		fmt.Sprintf("/api/v1/nodes/%d/versions?limit=1&offset=0", n.ID), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.ContentVersionPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Equal(t, 1, page.Total)
	assert.Equal(t, 1, page.Limit)
	require.Len(t, page.Items, 1)
	version := page.Items[0]
	assert.Equal(t, n.CurrentVersionID, version.ID)
	assert.Equal(t, n.ID, version.NodeID)
	assert.Equal(t, n.BlobHash, version.BlobHash)
	assert.Equal(t, "content_create", version.TransitionKind)
	assert.Equal(t, int64(1), version.NodeRevision)

	resp, body = get(t, ts, "/api/v1/versions/"+version.ID, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var byID api.ContentVersion
	require.NoError(t, json.Unmarshal([]byte(body), &byID))
	assert.Equal(t, version, byID)

	packed, err := s.Blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, packed.BlobsPacked)
	resp, body = get(t, ts, "/api/v1/versions/"+version.ID+"/content", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, "stable version bytes", body)
	assert.Equal(t, version.ID, resp.Header.Get(api.ContentVersionHeader))
	assert.Equal(t, version.BlobHash, resp.Header.Get(api.BlobHashHeader))
	assert.NotEmpty(t, resp.Trailer.Get("Content-Digest"))

	resp, body = get(t, ts, "/api/v1/nodes/"+strconv.FormatInt(s.RootID(), 10)+"/versions", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, `"code":"not_file"`)
}

func TestContentReferenceLookupIsLogicalPaginatedAndRepresentationNeutral(t *testing.T) {
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	wantedHash, wantedSize, err := s.Blobs.Write(strings.NewReader("shared content"))
	require.NoError(t, err)
	replacementHash, replacementSize, err := s.Blobs.Write(strings.NewReader("replacement"))
	require.NoError(t, err)

	historical, err := s.CreateFile(
		ctx, s.RootID(), "historical.txt", wantedHash, wantedSize, "text/plain",
	)
	require.NoError(t, err)
	historicalVersion := historical.CurrentVersionID
	historical, _, err = s.ReplaceContent(ctx, historical.ID, historical.Revision,
		replacementHash, replacementSize, "text/plain")
	require.NoError(t, err)
	current, err := s.CreateFile(ctx, s.RootID(), "current.txt", wantedHash, wantedSize, "text/plain")
	require.NoError(t, err)
	trashed, err := s.CreateFile(ctx, s.RootID(), "trashed.txt", wantedHash, wantedSize, "text/plain")
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, trashed.ID, trashed.Revision)
	require.NoError(t, err)

	lookup := "/api/v1/content-references?sha256=" + wantedHash + "&limit=10&offset=0"
	resp, body := get(t, ts, lookup, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var loose api.ContentReferencePage
	require.NoError(t, json.Unmarshal([]byte(body), &loose))
	assert.Equal(t, 3, loose.Total)
	require.Len(t, loose.Items, 3)
	assert.Equal(t, current.ID, loose.Items[0].Node.ID)
	assert.True(t, loose.Items[0].IsCurrent)
	assert.Equal(t, "/current.txt", loose.Items[0].Path)
	assert.Equal(t, historical.ID, loose.Items[1].Node.ID)
	assert.Equal(t, historicalVersion, loose.Items[1].Version.ID)
	assert.False(t, loose.Items[1].IsCurrent)
	assert.Equal(t, replacementHash, loose.Items[1].Node.BlobHash)
	assert.Equal(t, trashed.ID, loose.Items[2].Node.ID)
	assert.NotEmpty(t, loose.Items[2].Node.TrashedAt)
	assert.Empty(t, loose.Items[2].Path)

	resp, body = get(t, ts,
		"/api/v1/content-references?sha256="+wantedHash+"&limit=1&offset=1", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.ContentReferencePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Equal(t, 3, page.Total)
	require.Len(t, page.Items, 1)
	assert.Equal(t, historicalVersion, page.Items[0].Version.ID)

	packed, err := s.Blobs.Maintainer().Pack(ctx, packstore.PackOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, packed.BlobsPacked)
	resp, body = get(t, ts, lookup, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var afterPack api.ContentReferencePage
	require.NoError(t, json.Unmarshal([]byte(body), &afterPack))
	assert.Equal(t, loose, afterPack, "physical representation cannot change logical lookup")

	resp, body = get(t, ts, "/api/v1/content-references?sha256=ABC", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, `"code":"validation"`)
}

func TestVerifyNodeContentBindsRevisionAndReadsStoredBytes(t *testing.T) {
	ts, s := newTestServer(t, nil)
	n := createFileWithContent(t, ts, s, "/evidence.txt", "evidence")
	_, etag := etagOf(t, ts, n.ID)
	path := fmt.Sprintf("/api/v1/nodes/%d/verify", n.ID)

	resp, body := do(t, ts, http.MethodPost, path, map[string]string{"If-Match": etag}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var report api.ContentVerification
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	assert.Equal(t, n.ID, report.NodeID)
	assert.Equal(t, n.CurrentVersionID, report.VersionID)
	assert.Equal(t, n.Revision, report.Revision)
	assert.Equal(t, n.BlobHash, report.BlobHash)
	assert.Equal(t, n.BlobHash, report.ComputedHash)
	assert.Equal(t, n.Size, report.Size)
	assert.Equal(t, n.Size, report.ComputedSize)
	assert.True(t, report.Verified)
	assert.Empty(t, report.Problem)

	resp, body = do(t, ts, http.MethodPost, path, nil, nil)
	assert.Equal(t, http.StatusPreconditionRequired, resp.StatusCode)
	assert.Contains(t, body, `"code":"precondition_required"`)
	resp, body = do(t, ts, http.MethodPost, path,
		map[string]string{"If-Match": `"999"`}, nil)
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode)
	assert.Contains(t, body, `"code":"stale_revision"`)

	corrupt := []byte("damaged!")
	require.Len(t, corrupt, int(n.Size))
	require.NoError(t, os.WriteFile(filepath.Join(s.BlobsDir, n.BlobHash[:2], n.BlobHash), corrupt, 0o600))
	resp, body = do(t, ts, http.MethodPost, path, map[string]string{"If-Match": etag}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	report = api.ContentVerification{}
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	assert.False(t, report.Verified)
	assert.Equal(t, "corrupt", report.Problem)
	assert.NotEqual(t, report.BlobHash, report.ComputedHash)
	assert.Equal(t, report.Size, report.ComputedSize)

	require.NoError(t, s.Blobs.Remove(n.BlobHash))
	resp, body = do(t, ts, http.MethodPost, path, map[string]string{"If-Match": etag}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	report = api.ContentVerification{}
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	assert.False(t, report.Verified)
	assert.Equal(t, "missing", report.Problem)
}

func TestVerifyNodeContentRejectsDirectory(t *testing.T) {
	ts, s := newTestServer(t, nil)
	d, err := s.Mkdir(t.Context(), s.RootID(), "directory")
	require.NoError(t, err)
	_, etag := etagOf(t, ts, d.ID)
	resp, body := do(t, ts, http.MethodPost,
		fmt.Sprintf("/api/v1/nodes/%d/verify", d.ID), map[string]string{"If-Match": etag}, nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, `"code":"not_file"`)
}

func TestContentOnDirIs422(t *testing.T) {
	ts, s := newTestServer(t, nil)
	d, err := s.Mkdir(t.Context(), s.RootID(), "d")
	require.NoError(t, err)
	resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/content", d.ID), nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, `"code":"not_file"`)
}

func TestSearch(t *testing.T) {
	ts, s := newTestServer(t, nil)
	for i, name := range []string{"insurance-a.pdf", "insurance-b.pdf"} {
		_, err := s.CreateFile(t.Context(), s.RootID(), name, testHash(string(rune('x'+i))), 3, "application/pdf")
		require.NoError(t, err)
	}
	resp, body := get(t, ts, "/api/v1/search?q=insurance&limit=1", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var rep api.SearchReport
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	assert.Len(t, rep.Hits, 1)
	assert.Equal(t, 1, rep.Limit)
	assert.True(t, rep.Truncated)
}
