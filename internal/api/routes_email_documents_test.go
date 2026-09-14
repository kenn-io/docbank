package api_test

import (
	"context"
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
	"io"
	"net/http"
	"strings"
	"testing"
)

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

func TestEmailDocumentConsentErrorStatuses(t *testing.T) {
	for _, test := range []struct {
		code   string
		cause  error
		status int
	}{
		{"processing_consent_required", store.ErrProcessingConsentRequired, http.StatusPreconditionRequired},
		{"processing_consent_expired", store.ErrProcessingConsentExpired, http.StatusPreconditionFailed},
		{"processing_consent_revoked", store.ErrProcessingConsentRevoked, http.StatusPreconditionFailed},
	} {
		t.Run(test.code, func(t *testing.T) {
			ts, _ := newTestServer(t, func(d *api.Deps) {
				d.PublishEmailDocuments = func(context.Context, *store.Store, *blob.Store, document.EmailDocumentPublicationRequest) (document.EmailDocumentPublicationReceipt, error) {
					return document.EmailDocumentPublicationReceipt{}, test.cause
				}
			})
			response, body := do(t, ts, http.MethodPost, "/api/v1/email-document-publications", nil, struct{}{})
			require.Equal(t, test.status, response.StatusCode, body)
			var problem api.Error
			require.NoError(t, json.Unmarshal([]byte(body), &problem))
			require.Equal(t, test.code, problem.Code)
		})
	}
}

func TestEmailDocumentProcessingRejectsInvalidJob(t *testing.T) {
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	ts, s := newTestServer(t, configureProcessingTestServiceWithProvider(t, provider))
	c := client.New(ts.URL, testAPIKey)
	raw := "Content-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: text/plain\r\n\r\nbody\r\n--m\r\nContent-Type: text/csv\r\nContent-Disposition: attachment; filename=table.csv\r\n\r\nitem,count\nsynthetic,1\r\n--m--\r\n"
	uploaded, err := c.Upload(t.Context(), s.RootID(), "processing.eml", "message/rfc822", testHash(raw), int64(len(raw)), strings.NewReader(raw))
	require.NoError(t, err)
	view, err := c.EnsureEmailMetadata(t.Context(), uploaded.Node.CurrentVersionID)
	require.NoError(t, err)
	dir, err := s.NodeByID(t.Context(), s.RootID())
	require.NoError(t, err)
	receipt, err := c.PublishEmailDocuments(t.Context(), document.EmailDocumentPublicationRequest{
		OperationID: "invalid-processing-job", Parent: document.EmailDocumentIdentity{NodeID: view.Version.NodeID, VersionID: view.Version.ID, SHA256: view.Version.BlobHash, Size: view.Version.Size},
		GenerationID: view.GenerationID, AttachmentID: view.AttachmentID, DestinationID: dir.ID, DestinationRevision: dir.Revision,
	})
	require.NoError(t, err)
	request := document.EmailDocumentProcessingRequest{
		OperationID: receipt.OperationID, RequestDigest: receipt.RequestDigest, Order: 1,
		Profile: processingTestProfile(provider.Descriptor()),
	}
	request.ExecutionIdentity.Upload.SHA256 = receipt.Relations[0].Child.SHA256
	request.ExecutionIdentity.Upload.ByteLength = receipt.Relations[0].Child.Size
	response, body := do(t, ts, http.MethodPost, "/api/v1/email-document-processing", nil, request)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	var problem api.Error
	require.NoError(t, json.Unmarshal([]byte(body), &problem))
	require.Equal(t, "validation", problem.Code)

	request.Profile.Rendition.MaxUnits++
	response, body = do(t, ts, http.MethodPost, "/api/v1/email-document-processing", nil, request)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &problem))
	require.Equal(t, "processing_profile_unavailable", problem.Code)

	request.Profile = processingTestProfile(provider.Descriptor())
	_, _, err = s.ReplaceContent(t.Context(), receipt.Relations[0].Child.NodeID,
		store.UnconditionalRev, testHash("changed child"), int64(len("changed child")), "text/plain")
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodPost, "/api/v1/email-document-processing", nil, request)
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &problem))
	require.Equal(t, "email_document_conflict", problem.Code)
	require.Contains(t, problem.Detail, receipt.OperationID)
}

func TestEmailDocumentProcessingRequiresConfiguredService(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	response, body := do(t, ts, http.MethodPost, "/api/v1/email-document-processing", nil, struct{}{})
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode, body)
	var problem api.Error
	require.NoError(t, json.Unmarshal([]byte(body), &problem))
	require.Equal(t, "processing_unavailable", problem.Code)
}
