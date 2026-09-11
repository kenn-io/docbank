// internal/api/routes_read_test.go
package api_test

import (
	"crypto/md5" //nolint:gosec // Test coverage for explicitly auxiliary interoperability metadata.
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
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

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
)

func TestBrowserSourceMetadataOmission(t *testing.T) {
	ts, s := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var issued struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &issued))
	require.NotEmpty(t, issued.Token)
	for _, name := range []string{"bcc", "bcc-warning", "gps", "ordinary", "warning"} {
		t.Run(name, func(t *testing.T) {
			payload := "From: synthetic@example.test\r\nSubject: ordinary title\r\n\r\nbody"
			if name == "bcc" {
				payload = "Bcc: private-marker@example.test\r\nSubject: " + strings.Repeat("x", document.MaxSourceMetadataValueBytes+1) + "\r\n\r\nbody"
			}
			if name == "bcc-warning" {
				payload = "Bcc: " + strings.Repeat("x", document.MaxSourceMetadataValueBytes+1) + "\r\n\r\nbody"
			}
			if name == "gps" {
				payload = string(browserMetadataJPEG())
			}
			if name == "warning" {
				payload = "unsupported synthetic bytes"
			}
			receipt, err := s.Blobs.WriteDetailedContext(t.Context(), strings.NewReader(payload))
			require.NoError(t, err)
			encoding, err := receipt.EncodingName()
			require.NoError(t, err)
			ingest, err := s.BeginIngest(t.Context(), "cli", "/synthetic/private-attachment-marker")
			require.NoError(t, err)
			node, _, err := s.IngestFile(t.Context(), ingest, s.RootID(), name+".eml", receipt.Hash, receipt.Size, "application/octet-stream", "/synthetic/private-attachment-marker/"+name, "2024-01-02T03:04:05Z", store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize, PackEligible: receipt.PackEligible, Created: receipt.Created, MD5: receipt.MD5})
			require.NoError(t, err)
			metadata := processing.ExtractSourceMetadata([]byte(payload))
			canonical, _, err := document.MarshalSourceMetadataV1(metadata)
			require.NoError(t, err)
			_, err = s.PublishSourceMetadata(t.Context(), node.BlobHash, processing.SourceMetadataExtractorFingerprint, canonical)
			require.NoError(t, err)
			for _, route := range []string{fmt.Sprintf("/api/v1/nodes/%d", node.ID), "/api/v1/path?path=%2F" + name + ".eml"} {
				master, masterBody := get(t, ts, route, nil)
				require.Equal(t, http.StatusOK, master.StatusCode)
				require.Contains(t, masterBody, `"source_metadata"`)
				browser, browserBody := get(t, ts, route, map[string]string{"X-Api-Key": "", api.WebSessionHeader: issued.Token})
				require.Equal(t, http.StatusOK, browser.StatusCode)
				assert.NotContains(t, browserBody, `"source_metadata"`)
				assert.NotContains(t, browserBody, "private-marker")
				assert.NotContains(t, browserBody, "private-attachment-marker")
				assert.NotContains(t, browserBody, "51.5000000")
				assert.NotContains(t, browserBody, "-0.1000000")
				assert.NotContains(t, browserBody, "image.exif.gps")
				assert.NotContains(t, browserBody, "embedded value was omitted")
				assert.Equal(t, master.Header.Get("ETag"), browser.Header.Get("ETag"))
				var masterNode, browserNode api.Node
				require.NoError(t, json.Unmarshal([]byte(masterBody), &masterNode))
				require.NoError(t, json.Unmarshal([]byte(browserBody), &browserNode))
				require.NotNil(t, masterNode.SourceMetadata)
				assert.ElementsMatch(t, metadata.Fields, masterNode.SourceMetadata.Fields)
				assert.ElementsMatch(t, metadata.Warnings, masterNode.SourceMetadata.Warnings)
				assert.Contains(t, masterNode.SourceMetadata.Attachment.SourcePath, "private-attachment-marker")
				masterNode.SourceMetadata = nil
				assert.Equal(t, masterNode, browserNode)
			}
			headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issued.Token}
			resp, body := get(t, ts, "/api/v1/versions/"+node.CurrentVersionID, headers)
			assert.Equal(t, http.StatusForbidden, resp.StatusCode, body)
			resp, body = get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/versions", node.ID), headers)
			assert.Equal(t, http.StatusOK, resp.StatusCode, body)
			assert.NotContains(t, body, "source_metadata")
			resp, body = get(t, ts, "/api/v1/versions/"+node.CurrentVersionID, nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			assert.Contains(t, body, "source_metadata")
			db, err := s.SQLiteDriver().Open(s.DBPath, docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			_, err = db.ExecContext(t.Context(), `DROP TRIGGER IF EXISTS source_metadata_generations_immutable_update`)
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), `UPDATE source_metadata_generations SET checksum=? WHERE source_sha256=?`, strings.Repeat("0", 64), node.BlobHash)
			require.NoError(t, err)
			require.NoError(t, db.Close())
			for _, principal := range []map[string]string{nil, headers} {
				resp, body = get(t, ts, fmt.Sprintf("/api/v1/nodes/%d", node.ID), principal)
				assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, body)
			}
		})
	}
}

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

func TestStatAndContentVersionDetailExposeActiveSourceMetadata(t *testing.T) {
	ts, s := newTestServer(t, nil)
	node, err := s.CreateFile(t.Context(), s.RootID(), "report.pdf", testHash("metadata"), 9, "application/pdf")
	require.NoError(t, err)
	canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{
		ContractVersion: document.SourceMetadataContractV1,
		Fields: []document.SourceMetadataFieldV1{{Key: "title", Namespace: "pdf.info", SourceField: "Title",
			Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: "Synthetic report"}}}})
	require.NoError(t, err)
	_, err = s.PublishSourceMetadata(t.Context(), node.BlobHash, testHash("extractor"), canonical)
	require.NoError(t, err)

	for _, path := range []string{fmt.Sprintf("/api/v1/nodes/%d", node.ID), "/api/v1/versions/" + node.CurrentVersionID} {
		resp, body := get(t, ts, path, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		assert.Contains(t, body, `"source_metadata"`)
		assert.Contains(t, body, "Synthetic report")
		assert.Contains(t, body, `"filename":"report.pdf"`)
	}
}

func TestStatTrashedNodeHasNoLivePath(t *testing.T) {
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	node, err := s.Mkdir(ctx, s.RootID(), "archived")
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, node.ID, node.Revision)
	require.NoError(t, err)

	resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d", node.ID), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var got api.Node
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	assert.Equal(t, node.ID, got.ID)
	assert.NotEmpty(t, got.TrashedAt)
	assert.Empty(t, got.Path)
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
		Directory api.Node   `json:"directory"`
		Items     []api.Node `json:"items"`
		Total     int        `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Equal(t, 5, page.Total)
	assert.Len(t, page.Items, 1) // offset 4 of 5
	assert.Equal(t, s.RootID(), page.Directory.ID)
	assert.Equal(t, "/", page.Directory.Path)
}

func TestChildrenRefreshReturnsCurrentDirectoryAuthority(t *testing.T) {
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	dir, err := s.Mkdir(ctx, s.RootID(), "old")
	require.NoError(t, err)
	_, err = s.Mkdir(ctx, dir.ID, "child")
	require.NoError(t, err)

	_, _, err = s.Move(ctx, dir.ID, s.RootID(), "current", store.UnconditionalRev)
	require.NoError(t, err)
	resp, body := get(t, ts,
		fmt.Sprintf("/api/v1/nodes/%d/children?limit=10&offset=0", dir.ID), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.NodePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Equal(t, dir.ID, page.Directory.ID)
	assert.Equal(t, "/current", page.Directory.Path)
	require.Len(t, page.Items, 1)

	current, err := s.NodeByID(ctx, dir.ID)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, dir.ID, current.Revision)
	require.NoError(t, err)
	resp, body = get(t, ts,
		fmt.Sprintf("/api/v1/nodes/%d/children?limit=10&offset=0", dir.ID), nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"not_found"`)
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
	n := createFileWithChecksum(t, s, "identity.txt", "identity")
	resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d", n.ID), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got api.Node
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	assert.Equal(t, n.BlobHash, got.BlobHash)
	md5sum := md5.Sum([]byte("identity")) //nolint:gosec // Explicit auxiliary MD5 assertion.
	assert.Equal(t, hex.EncodeToString(md5sum[:]), got.MD5)
	assert.Equal(t, n.Size, got.Size)
	assert.Equal(t, n.CurrentVersionID, got.CurrentVersionID)
}

func TestContentVersionListMetadataAndPackedBytes(t *testing.T) {
	ts, s := newTestServer(t, nil)
	n := createFileWithChecksum(t, s, "versioned.txt", "stable version bytes")
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
	md5sum := md5.Sum([]byte("stable version bytes")) //nolint:gosec // Explicit auxiliary MD5 assertion.
	assert.Equal(t, hex.EncodeToString(md5sum[:]), version.MD5)
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

func createFileWithChecksum(t *testing.T, s *testStore, name, content string) store.Node {
	t.Helper()
	receipt, err := s.Blobs.WriteDetailedContext(t.Context(), strings.NewReader(content))
	require.NoError(t, err)
	encoding, err := receipt.EncodingName()
	require.NoError(t, err)
	node, err := s.CreateFile(t.Context(), s.RootID(), name, receipt.Hash, receipt.Size,
		"text/plain", store.BlobPhysical{
			Encoding: encoding, StoredBytes: receipt.StoredSize,
			PackEligible: receipt.PackEligible, Created: receipt.Created, MD5: receipt.MD5,
		})
	require.NoError(t, err)
	return node
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
	var insuranceNodes []store.Node
	for i, name := range []string{"insurance-a.pdf", "insurance-b.pdf"} {
		node, err := s.CreateFile(t.Context(), s.RootID(), name, testHash(string(rune('x'+i))), 3, "application/pdf")
		require.NoError(t, err)
		insuranceNodes = append(insuranceNodes, node)
	}
	resp, body := get(t, ts, "/api/v1/search?q=insurance&limit=1", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var rep api.SearchReport
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	assert.Len(t, rep.Hits, 1)
	assert.Equal(t, 1, rep.Limit)
	assert.True(t, rep.Truncated)
	assert.Equal(t, "name", rep.Hits[0].Match)

	resp, body = get(t, ts, "/api/v1/search?q=insurance&limit=10&"+
		"modified_since=2000-01-01T00:00:00-05:00&modified_before=2100-01-01T00:00:00Z", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	require.Len(t, rep.Hits, 2)
	assert.Equal(t, "2000-01-01T05:00:00.000000000Z", rep.ModifiedSince)
	assert.Equal(t, "2100-01-01T00:00:00.000000000Z", rep.ModifiedBefore)
	resp, body = get(t, ts, "/api/v1/search?q=insurance&limit=10&"+
		"modified_since=2100-01-01T00:00:00Z&modified_before=2000-01-01T00:00:00Z", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"validation"`)

	tag, err := s.CreateTag(t.Context(), "renewal")
	require.NoError(t, err)
	_, err = s.AssignTag(
		t.Context(), tag.ID, insuranceNodes[1].ID, insuranceNodes[1].Revision,
	)
	require.NoError(t, err)
	resp, body = get(t, ts, "/api/v1/search?q=insurance&limit=10&tag_id="+tag.ID, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	require.Len(t, rep.Hits, 1)
	assert.Equal(t, insuranceNodes[1].ID, rep.Hits[0].Node.ID)
	assert.Equal(t, tag.ID, rep.TagID)

	resp, body = get(t, ts,
		"/api/v1/search?q=insurance&limit=10&tag_id=11111111-1111-4111-8111-111111111111", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, body)

	archive, err := s.Mkdir(t.Context(), s.RootID(), "archive")
	require.NoError(t, err)
	inside, err := s.CreateFile(
		t.Context(), archive.ID, "insurance-archive.pdf", testHash("scope"), 3, "application/pdf",
	)
	require.NoError(t, err)
	resp, body = get(t, ts, fmt.Sprintf(
		"/api/v1/search?q=insurance&limit=10&under_node_id=%d", archive.ID,
	), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	require.Len(t, rep.Hits, 1)
	assert.Equal(t, inside.ID, rep.Hits[0].Node.ID)
	assert.Equal(t, archive.ID, rep.UnderNodeID)
	resp, body = get(t, ts, fmt.Sprintf(
		"/api/v1/search?q=insurance&limit=10&under_node_id=%d", inside.ID,
	), nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"not_dir"`)

	bodyNode := createFileWithContent(t, ts, s, "/notes.txt", "lighthouse archive")
	require.NoError(t, s.RecordExtraction(t.Context(), store.ExtractionResult{
		BlobHash: bodyNode.BlobHash, Extractor: "plain-text", ExtractorVersion: 1,
		Status: store.ExtractionOK, Text: "lighthouse archive",
	}))
	resp, body = get(t, ts, "/api/v1/search?q=lighthouse&limit=10", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	require.Len(t, rep.Hits, 1)
	assert.Equal(t, bodyNode.ID, rep.Hits[0].Node.ID)
	assert.Equal(t, "content", rep.Hits[0].Match)

	resp, body = get(t, ts, "/api/v1/search?q=lighthouse&limit=10&mime_type=TEXT%2FPLAIN", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	require.Len(t, rep.Hits, 1)
	assert.Equal(t, bodyNode.ID, rep.Hits[0].Node.ID)
	assert.Equal(t, "text/plain", rep.MIMEType)

	resp, body = get(t, ts,
		"/api/v1/search?q=lighthouse&limit=10&mime_type=text%2Fplain%3B%20charset%3Dutf-8", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"validation"`)
}

func browserMetadataJPEG() []byte {
	const (
		rootOffset        = 8
		descriptionOffset = 38
		gpsOffset         = 54
		latitudeOffset    = 108
		longitudeOffset   = 132
	)
	tiff := make([]byte, 156)
	copy(tiff, "II")
	binary.LittleEndian.PutUint16(tiff[2:], 42)
	binary.LittleEndian.PutUint32(tiff[4:], rootOffset)
	binary.LittleEndian.PutUint16(tiff[rootOffset:], 2)
	putEXIFEntry(tiff[rootOffset+2:], 0x010e, 2, 16, descriptionOffset)
	putEXIFEntry(tiff[rootOffset+14:], 0x8825, 4, 1, gpsOffset)
	copy(tiff[descriptionOffset:], "Synthetic image\x00")
	binary.LittleEndian.PutUint16(tiff[gpsOffset:], 4)
	putEXIFInlineASCII(tiff[gpsOffset+2:], 1, "N")
	putEXIFEntry(tiff[gpsOffset+14:], 2, 5, 3, latitudeOffset)
	putEXIFInlineASCII(tiff[gpsOffset+26:], 3, "W")
	putEXIFEntry(tiff[gpsOffset+38:], 4, 5, 3, longitudeOffset)
	putEXIFRationals(tiff[latitudeOffset:], [3]uint32{51, 30, 0})
	putEXIFRationals(tiff[longitudeOffset:], [3]uint32{0, 6, 0})
	segment := append([]byte("Exif\x00\x00"), tiff...)
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(jpeg[4:], uint16(len(segment)+2))
	jpeg = append(jpeg, segment...)
	return append(jpeg, 0xff, 0xd9)
}

func putEXIFEntry(target []byte, tag, kind uint16, count, value uint32) {
	binary.LittleEndian.PutUint16(target, tag)
	binary.LittleEndian.PutUint16(target[2:], kind)
	binary.LittleEndian.PutUint32(target[4:], count)
	binary.LittleEndian.PutUint32(target[8:], value)
}

func putEXIFInlineASCII(target []byte, tag uint16, value string) {
	putEXIFEntry(target, tag, 2, 2, 0)
	copy(target[8:12], value+"\x00")
}

func putEXIFRationals(target []byte, values [3]uint32) {
	for index, value := range values {
		binary.LittleEndian.PutUint32(target[index*8:], value)
		binary.LittleEndian.PutUint32(target[index*8+4:], 1)
	}
}
