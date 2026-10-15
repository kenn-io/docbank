package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

type uploadRequest struct {
	name         string
	filename     string
	content      []byte
	expectedHash string
	expectedSize int64
	extraPart    bool
	pushName     string
	sourceRef    string
	runID        string
	parentPath   string
}

func sendUpload(
	t *testing.T, tsURL string, client *http.Client, parentID int64, in uploadRequest,
) (*http.Response, string) {
	t.Helper()
	if in.filename == "" {
		in.filename = in.name
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", multipart.FileContentDisposition("file", in.filename))
	header.Set("Content-Type", "text/plain; charset=utf-8")
	part, err := writer.CreatePart(header)
	require.NoError(t, err)
	_, err = part.Write(in.content)
	require.NoError(t, err)
	if in.extraPart {
		require.NoError(t, writer.WriteField("extra", "not allowed"))
	}
	require.NoError(t, writer.Close())

	query := url.Values{"parent_id": {strconv.FormatInt(parentID, 10)}, "name": {in.name}}
	route := "/api/v1/uploads"
	if in.pushName != "" {
		route = "/api/v1/push/uploads"
		query.Del("parent_id")
		query.Set("parent_path", in.parentPath)
		if in.parentPath == "" {
			query.Set("parent_path", "/")
		}
		query.Set("push_name", in.pushName)
		query.Set("source_ref", in.sourceRef)
		query.Set("run_id", in.runID)
		if in.runID == "" {
			query.Set("run_id", uuid.NewV4().String())
		}
	}
	req, err := http.NewRequest(http.MethodPost, tsURL+route+"?"+query.Encode(), &body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set(api.BlobHashHeader, in.expectedHash)
	req.Header.Set(api.BlobSizeHeader, strconv.FormatInt(in.expectedSize, 10))
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var response bytes.Buffer
	_, err = response.ReadFrom(resp.Body)
	require.NoError(t, err)
	return resp, response.String()
}

func uploadIdentity(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func TestUploadCreatesReceiptAndIdempotentRetry(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	content := []byte("remote upload")
	in := uploadRequest{
		name: "report.txt", content: content,
		expectedHash: uploadIdentity(content), expectedSize: int64(len(content)),
	}

	resp, body := sendUpload(t, ts.URL, ts.Client(), s.RootID(), in)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var added api.UploadReceipt
	require.NoError(t, json.Unmarshal([]byte(body), &added))
	assert.NotContains(t, body, `"ingest_id"`)
	assert.Equal(t, "added", added.Status)
	assert.Equal(t, in.expectedHash, added.ComputedHash)
	assert.Equal(t, in.expectedSize, added.ComputedSize)
	assert.Equal(t, in.expectedHash, added.Node.BlobHash)
	assert.Equal(t, "text/plain; charset=utf-8", added.Node.MimeType)

	stored, err := s.NodeByPath(t.Context(), "/report.txt")
	require.NoError(t, err)
	assert.Equal(t, added.Node.ID, stored.ID)

	resp, body = sendUpload(t, ts.URL, ts.Client(), s.RootID(), in)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var skipped api.UploadReceipt
	require.NoError(t, json.Unmarshal([]byte(body), &skipped))
	assert.NotContains(t, body, `"ingest_id"`)
	assert.Equal(t, "skipped", skipped.Status)
	assert.Equal(t, added.Node.ID, skipped.Node.ID)
	assert.Equal(t, added.Node.Revision, skipped.Node.Revision)

	changed := []byte("changed content")
	in.content = changed
	in.expectedHash = uploadIdentity(changed)
	in.expectedSize = int64(len(changed))
	resp, body = sendUpload(t, ts.URL, ts.Client(), s.RootID(), in)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var suffixed api.UploadReceipt
	require.NoError(t, json.Unmarshal([]byte(body), &suffixed))
	assert.Equal(t, "report (2).txt", suffixed.Node.Name)
}

func TestUploadAcceptsEmptyFile(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	content := []byte{}
	in := uploadRequest{name: "empty.txt", content: content,
		expectedHash: uploadIdentity(content), expectedSize: 0}
	resp, body := sendUpload(t, ts.URL, ts.Client(), s.RootID(), in)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var receipt api.UploadReceipt
	require.NoError(t, json.Unmarshal([]byte(body), &receipt))
	assert.Zero(t, receipt.ComputedSize)
	assert.Equal(t, in.expectedHash, receipt.ComputedHash)
}

func TestUploadRejectsDeclaredSizeAboveIngestLimit(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	content := []byte("small envelope")
	in := uploadRequest{name: "too-large.bin", content: content,
		expectedHash: uploadIdentity(content), expectedSize: blob.MaxIngestBytes + 1}
	resp, body := sendUpload(t, ts.URL, ts.Client(), s.RootID(), in)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, strconv.FormatInt(blob.MaxIngestBytes, 10))
	_, err := s.NodeByPath(t.Context(), "/too-large.bin")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestUploadRejectsMismatchedEvidenceWithoutAuthority(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		mutate     func(*uploadRequest)
		wantCode   string
		wantStatus int
	}{
		{name: "digest", mutate: func(in *uploadRequest) { in.expectedHash = uploadIdentity([]byte("other")) },
			wantCode: "digest_mismatch", wantStatus: http.StatusUnprocessableEntity},
		{name: "short size", mutate: func(in *uploadRequest) { in.expectedSize-- },
			wantCode: "size_mismatch", wantStatus: http.StatusUnprocessableEntity},
		{name: "long size", mutate: func(in *uploadRequest) { in.expectedSize++ },
			wantCode: "size_mismatch", wantStatus: http.StatusUnprocessableEntity},
		{name: "extra part", mutate: func(in *uploadRequest) { in.extraPart = true },
			wantCode: "validation", wantStatus: http.StatusUnprocessableEntity},
		{name: "different filename", mutate: func(in *uploadRequest) { in.filename = "other.txt" },
			wantCode: "validation", wantStatus: http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, s := newTestServer(t, nil)
			content := []byte("remote upload")
			in := uploadRequest{name: "rejected.txt", content: content,
				expectedHash: uploadIdentity(content), expectedSize: int64(len(content))}
			tt.mutate(&in)
			resp, body := sendUpload(t, ts.URL, ts.Client(), s.RootID(), in)
			assert.Equal(t, tt.wantStatus, resp.StatusCode, body)
			assert.Contains(t, body, fmt.Sprintf(`"code":%q`, tt.wantCode))
			_, err := s.NodeByPath(t.Context(), "/rejected.txt")
			require.ErrorIs(t, err, store.ErrNotFound)
			blobs, err := s.AllBlobs(t.Context())
			require.NoError(t, err)
			assert.Empty(t, blobs, "failed upload must grant no blob authority")
		})
	}
}

func TestUploadRejectsNonDirectoryBeforeReadingContent(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	parent := createFileWithContent(t, ts, s, "/not-a-dir", "existing")
	content := []byte("remote upload")
	in := uploadRequest{name: "child.txt", content: content,
		expectedHash: uploadIdentity(content), expectedSize: int64(len(content))}
	resp, body := sendUpload(t, ts.URL, ts.Client(), parent.ID, in)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"not_dir"`)
}

func TestUploadRejectsTruncatedMultipartAsValidation(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	content := "truncated"
	boundary := "missing-closing-boundary"
	body := fmt.Sprintf("--%s\r\nContent-Disposition: form-data; name=\"file\"; filename=\"bad.txt\"\r\n"+
		"Content-Type: text/plain\r\n\r\n%s", boundary, content)
	query := url.Values{"parent_id": {strconv.FormatInt(s.RootID(), 10)}, "name": {"bad.txt"}}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/uploads?"+query.Encode(), bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set(api.BlobHashHeader, uploadIdentity([]byte(content)))
	req.Header.Set(api.BlobSizeHeader, strconv.Itoa(len(content)))
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var response bytes.Buffer
	_, err = response.ReadFrom(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, response.String())
	assert.Contains(t, response.String(), `"code":"validation"`)
	_, err = s.NodeByPath(t.Context(), "/bad.txt")
	require.ErrorIs(t, err, store.ErrNotFound)
	blobs, err := s.AllBlobs(t.Context())
	require.NoError(t, err)
	assert.Empty(t, blobs)
}

func TestPushUploadIdentityVersionsAndIdempotence(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	in := uploadRequest{name: "report.txt", pushName: "laptop", sourceRef: "report.txt", content: []byte("first")}
	in.expectedHash, in.expectedSize = uploadIdentity(in.content), int64(len(in.content))
	resp, body := sendUpload(t, ts.URL, ts.Client(), s.RootID(), in)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var first api.PushUploadReceipt
	require.NoError(t, json.Unmarshal([]byte(body), &first))
	resp, body = sendUpload(t, ts.URL, ts.Client(), s.RootID(), in)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var repeated api.PushUploadReceipt
	require.NoError(t, json.Unmarshal([]byte(body), &repeated))
	assert.Equal(t, first.Node.ID, repeated.Node.ID)
	assert.Equal(t, first.Node.Revision, repeated.Node.Revision)
	in.content = []byte("second")
	in.expectedHash, in.expectedSize = uploadIdentity(in.content), int64(len(in.content))
	resp, body = sendUpload(t, ts.URL, ts.Client(), s.RootID(), in)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var changed api.PushUploadReceipt
	require.NoError(t, json.Unmarshal([]byte(body), &changed))
	assert.Equal(t, "updated", changed.Status)
	assert.Equal(t, first.Node.ID, changed.Node.ID)
	assert.NotEqual(t, first.Node.CurrentVersionID, changed.Node.CurrentVersionID)
}

func TestPushUploadGivesIdenticalSourcesSeparateNodesInOneRun(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	runID := uuid.NewV4().String()
	nodes := map[int64]bool{}
	for _, name := range []string{"first.txt", "second.txt"} {
		in := uploadRequest{name: name, pushName: "laptop", sourceRef: name, runID: runID, content: []byte("same bytes")}
		in.expectedHash, in.expectedSize = uploadIdentity(in.content), int64(len(in.content))
		resp, body := sendUpload(t, ts.URL, ts.Client(), 0, in)
		require.Equal(t, http.StatusCreated, resp.StatusCode, body)
		var receipt api.PushUploadReceipt
		require.NoError(t, json.Unmarshal([]byte(body), &receipt))
		assert.Equal(t, "added", receipt.Status)
		nodes[receipt.Node.ID] = true
	}
	assert.Len(t, nodes, 2, "identical bytes from two sources make two documents")
	_, total, err := s.Collections(t.Context(), 100, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, total, "one run is one collection")

	in := uploadRequest{name: "third.txt", pushName: "desktop", sourceRef: "third.txt", runID: runID, content: []byte("other")}
	in.expectedHash, in.expectedSize = uploadIdentity(in.content), int64(len(in.content))
	resp, body := sendUpload(t, ts.URL, ts.Client(), 0, in)
	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
	in.runID = "not-a-uuid"
	resp, body = sendUpload(t, ts.URL, ts.Client(), 0, in)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, "push run ID")
}

func TestPushSourcesPageThroughCursors(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	runID := uuid.NewV4().String()
	for _, name := range []string{"c.txt", "a.txt", "b.txt"} {
		in := uploadRequest{name: name, pushName: "laptop", sourceRef: name, runID: runID, content: []byte(name)}
		in.expectedHash, in.expectedSize = uploadIdentity(in.content), int64(len(in.content))
		resp, body := sendUpload(t, ts.URL, ts.Client(), 0, in)
		require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	}
	var refs []string
	after := ""
	for range 3 {
		resp, body := get(t, ts, "/api/v1/push/sources?push_name=laptop&limit=2&after="+url.QueryEscape(after), nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		var page api.PushSourcePage
		require.NoError(t, json.Unmarshal([]byte(body), &page))
		for _, cursor := range page.Items {
			refs = append(refs, cursor.SourceRef)
			assert.Equal(t, uploadIdentity([]byte(cursor.SourceRef)), cursor.Hash)
		}
		if page.Next == "" {
			break
		}
		after = page.Next
	}
	assert.Equal(t, []string{"a.txt", "b.txt", "c.txt"}, refs)
	resp, body := get(t, ts, "/api/v1/push/sources?push_name=Laptop", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
}

func TestPushUploadMismatchGrantsNoSourceAuthority(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	in := uploadRequest{name: "bad.txt", pushName: "laptop", sourceRef: "bad.txt", content: []byte("bytes"), expectedHash: uploadIdentity([]byte("other")), expectedSize: 5}
	resp, body := sendUpload(t, ts.URL, ts.Client(), s.RootID(), in)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, "digest_mismatch")
	blobs, err := s.AllBlobs(t.Context())
	require.NoError(t, err)
	assert.Empty(t, blobs)
}

func TestPushUploadCreatesItsDestinationOnlyForANewNode(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	in := uploadRequest{name: "notes.txt", pushName: "laptop", sourceRef: "a/notes.txt", parentPath: "/archive/a", content: []byte("first")}
	in.expectedHash, in.expectedSize = uploadIdentity(in.content), int64(len(in.content))
	resp, body := sendUpload(t, ts.URL, ts.Client(), 0, in)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	created, err := s.NodeByPath(t.Context(), "/archive/a/notes.txt")
	require.NoError(t, err)

	moved, err := s.MkdirAll(t.Context(), "/moved")
	require.NoError(t, err)
	_, _, err = s.Move(t.Context(), created.ID, moved.ID, "notes.txt", store.UnconditionalRev)
	require.NoError(t, err)
	archive, err := s.NodeByPath(t.Context(), "/archive")
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), archive.ID, archive.Revision)
	require.NoError(t, err)
	in.content = []byte("second")
	in.expectedHash, in.expectedSize = uploadIdentity(in.content), int64(len(in.content))
	resp, body = sendUpload(t, ts.URL, ts.Client(), 0, in)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var receipt api.PushUploadReceipt
	require.NoError(t, json.Unmarshal([]byte(body), &receipt))
	assert.Equal(t, "updated", receipt.Status)
	assert.Equal(t, created.ID, receipt.Node.ID)
	_, err = s.NodeByPath(t.Context(), "/archive")
	require.ErrorIs(t, err, store.ErrNotFound, "a stale destination is neither required nor recreated")

	in.parentPath = "relative"
	in.sourceRef = "other.txt"
	in.name = "other.txt"
	resp, body = sendUpload(t, ts.URL, ts.Client(), 0, in)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, "absolute virtual path")
}

func TestPushRoutesRefuseTheNameOfAConfiguredWatch(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Watches = []config.WatchConfig{{Name: "laptop", Source: t.TempDir(), Destination: "/watched"}}
	})
	resp, body := get(t, ts, "/api/v1/push/sources?push_name=laptop", nil)
	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
	assert.Contains(t, body, "push_name_in_use")
	in := uploadRequest{name: "notes.txt", pushName: "laptop", sourceRef: "notes.txt", content: []byte("bytes")}
	in.expectedHash, in.expectedSize = uploadIdentity(in.content), int64(len(in.content))
	resp, body = sendUpload(t, ts.URL, ts.Client(), 0, in)
	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
	assert.Contains(t, body, "push_name_in_use")
	resp, body = get(t, ts, "/api/v1/push/sources?push_name=desktop", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode, body)
	blobs, err := s.AllBlobs(t.Context())
	require.NoError(t, err)
	assert.Empty(t, blobs)
}
