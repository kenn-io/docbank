package api_test

import (
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestTrashEmptyReportsEmailRetentionWithoutBlockingOtherRoots(t *testing.T) {
	ts, s := newTestServer(t, nil)
	version := createEmailVersion(t, s, "synthetic-retained.eml", "Subject: Synthetic\r\n\r\nRetained body")
	response, body := do(t, ts, http.MethodPost, "/api/v1/versions/"+version.ID+"/email", nil, struct{}{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	view, err := s.EmailMetadata(t.Context(), version.ID)
	require.NoError(t, err)
	root, err := s.NodeByID(t.Context(), s.RootID())
	require.NoError(t, err)
	_, err = s.PublishEmailDocuments(t.Context(), document.EmailDocumentPublicationRequest{
		OperationID: "synthetic-retention", Parent: document.EmailDocumentIdentity{NodeID: version.NodeID, VersionID: version.ID, SHA256: version.BlobHash, Size: version.Size},
		GenerationID: view.Generation.ID, AttachmentID: view.Attachment.ID, DestinationID: root.ID, DestinationRevision: root.Revision,
	})
	require.NoError(t, err)
	discard := createFileWithContent(t, ts, s, "/synthetic-discard.txt", "Discard")
	for _, id := range []int64{version.NodeID, discard.ID} {
		_, _, err = s.Trash(t.Context(), id, store.UnconditionalRev)
		require.NoError(t, err)
	}
	response, body = do(t, ts, http.MethodPost, "/api/v1/trash/empty", nil, map[string]bool{"run": true})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var report api.TrashEmptyReport
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	require.EqualValues(t, 1, report.CandidateRoots)
	require.EqualValues(t, 1, report.Deleted)
	require.EqualValues(t, 1, report.RetainedRoots)
	_, err = s.NodeByID(t.Context(), version.NodeID)
	require.NoError(t, err)
	_, err = s.NodeByID(t.Context(), discard.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestEmailDocumentsAndConsentRequireMasterAPIKey(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	response, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var issued struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &issued))
	for _, path := range []string{"/api/v1/email-document-publications", "/api/v1/email-document-processing", "/api/v1/processing/consents", "/api/v1/processing/consents/revoke"} {
		response, body = do(t, ts, http.MethodPost, path, map[string]string{"X-Api-Key": "", api.WebSessionHeader: issued.Token}, struct{}{})
		require.Equal(t, http.StatusForbidden, response.StatusCode, path+": "+body)
		response, body = do(t, ts, http.MethodPost, path, map[string]string{"X-Api-Key": ""}, struct{}{})
		require.Equal(t, http.StatusUnauthorized, response.StatusCode, path+": "+body)
	}
}

func TestEmailDocumentRequestBoundaries(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	for _, path := range []string{"/api/v1/processing/consents", "/api/v1/processing/consents/revoke"} {
		response, body := do(t, ts, http.MethodPost, path, nil, struct{}{})
		require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	}
	postRaw := func(body string) (*http.Response, string) {
		r, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/email-document-publications", strings.NewReader(body))
		require.NoError(t, err)
		r.Header.Set("Content-Type", "application/json")
		resp, err := ts.Client().Do(r)
		require.NoError(t, err)
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		return resp, string(data)
	}
	for _, body := range []string{`{"unknown":1}`, `null`, `{"operation_id":"a","operation_id":"b"}`, `{"parent":{"node_id":9007199254740992}}`, `{"reuse":[{}]}`} {
		response, text := postRaw(body)
		require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, text)
	}
	for _, query := range []string{"limit=251", "child_version_id=nope", "unknown=1", "limit=1&limit=2", "limit=999999999999999999999999"} {
		response, body := get(t, ts, "/api/v1/email-document-relations?"+query, nil)
		require.Equal(t, 422, response.StatusCode, body)
	}
	response, body := postRaw(strings.Repeat(" ", 2<<20) + `{}`)
	require.Equal(t, 413, response.StatusCode, body)
}
