package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

type uploadRequest struct {
	name         string
	filename     string
	content      []byte
	expectedHash string
	expectedSize int64
	extraPart    bool
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
	req, err := http.NewRequest(http.MethodPost, tsURL+"/api/v1/uploads?"+query.Encode(), &body)
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

func TestUploadRejectsMismatchedEvidenceWithoutAuthority(t *testing.T) {
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
