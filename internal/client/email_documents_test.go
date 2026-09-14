package client_test

import (
	"crypto/sha256"
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestClientEmailDocumentProcessingConsentErrors(t *testing.T) {
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
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_ = json.MarshalWrite(w, api.NewError(test.status, test.code, test.cause.Error()))
			}))
			t.Cleanup(server.Close)
			_, err := client.New(server.URL, serverKey).RequestEmailDocumentProcessing(t.Context(), document.EmailDocumentProcessingRequest{})
			require.ErrorIs(t, err, client.ErrProcessingConsent)
			require.ErrorIs(t, err, test.cause)
		})
	}
}

func TestClientEmailDocumentsExplicitConsent(t *testing.T) {
	c, s := newClient(t, serverKey)
	r := document.ProcessingConsentRequest{Principal: "operator:synthetic", Scope: "attachment:synthetic", ProfileFingerprint: fmtHash(sha256.Sum256([]byte("profile"))), DisclosureFingerprint: fmtHash(sha256.Sum256([]byte("disclosure"))), InputClasses: []string{"original_source"}}
	a := store.ProviderOperationAuthorizationRequest{Principal: r.Principal, Scope: r.Scope, ProfileFingerprint: r.ProfileFingerprint, DisclosureFingerprint: r.DisclosureFingerprint, InputClasses: r.InputClasses}
	_, err := s.AuthorizeProviderOperation(t.Context(), a)
	require.ErrorIs(t, err, store.ErrProcessingConsentRequired)
	grant, err := c.GrantScopedProcessingConsent(t.Context(), r)
	require.NoError(t, err)
	authorized, err := s.AuthorizeProviderOperation(t.Context(), a)
	require.NoError(t, err)
	require.Equal(t, grant.GrantID, authorized.GrantID)
	revoked, err := c.RevokeScopedProcessingConsent(t.Context(), document.ProcessingConsentRevocationRequest{Principal: r.Principal, Scope: r.Scope})
	require.NoError(t, err)
	require.Greater(t, revoked.Fence, grant.RevocationFence)
	_, err = s.AuthorizeProviderOperation(t.Context(), a)
	require.ErrorIs(t, err, store.ErrProcessingConsentRevoked)
}

func TestClientEmailDocumentsRoundTrip(t *testing.T) {
	c, s := newClient(t, serverKey)
	raw := "Content-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: text/plain\r\n\r\nbody\r\n--m\r\nContent-Type: text/csv\r\nContent-Disposition: attachment; filename=table.csv\r\n\r\nitem,count\nsynthetic,1\r\n--m--\r\n"
	uploaded, err := c.Upload(t.Context(), s.RootID(), "client.eml", "message/rfc822", fmtHash(sha256.Sum256([]byte(raw))), int64(len(raw)), strings.NewReader(raw))
	require.NoError(t, err)
	view, err := c.EnsureEmailMetadata(t.Context(), uploaded.Node.CurrentVersionID)
	require.NoError(t, err)
	dir, err := s.NodeByID(t.Context(), s.RootID())
	require.NoError(t, err)
	request := document.EmailDocumentPublicationRequest{OperationID: "client-publication", Parent: document.EmailDocumentIdentity{NodeID: view.Version.NodeID, VersionID: view.Version.ID, SHA256: view.Version.BlobHash, Size: view.Version.Size}, GenerationID: view.GenerationID, AttachmentID: view.AttachmentID, DestinationID: dir.ID, DestinationRevision: dir.Revision, Reuse: []document.EmailDocumentReuse{}}
	receipt, err := c.PublishEmailDocuments(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, receipt.Relations, 1)
	retry, err := c.PublishEmailDocuments(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, receipt, retry)
	read, err := c.EmailDocumentPublication(t.Context(), receipt.OperationID)
	require.NoError(t, err)
	require.Equal(t, receipt, read)
	page, err := c.EmailDocumentRelations(t.Context(), document.EmailDocumentRelationQuery{ChildVersionID: receipt.Relations[0].Child.VersionID})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, request.Parent, page.Items[0].Relation.Parent)

	for _, failure := range []struct {
		name      string
		body      string
		truncated bool
	}{
		{"read", `{"operation_id":`, true},
		{"decode", `{"operation_id":`, false},
		{"strict_json", `{"unknown":true}`, false},
		{"byte_limit", strings.Repeat(" ", document.EmailDocumentMaxJSONBytes+1), false},
	} {
		t.Run("committed_response_"+failure.name, func(t *testing.T) {
			request := request
			request.OperationID += "-" + failure.name
			dir, err := s.NodeByID(t.Context(), s.RootID())
			require.NoError(t, err)
			request.DestinationRevision = dir.Revision
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var submitted document.EmailDocumentPublicationRequest
				if err := json.UnmarshalRead(r.Body, &submitted); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if _, err := c.PublishEmailDocuments(r.Context(), submitted); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if failure.truncated {
					w.Header().Set("Content-Length", strconv.Itoa(len(failure.body)+100))
				}
				_, _ = io.WriteString(w, failure.body)
			}))
			t.Cleanup(server.Close)
			_, err = client.New(server.URL, serverKey).PublishEmailDocuments(t.Context(), request)
			require.Error(t, err)
			committed, readErr := c.EmailDocumentPublication(t.Context(), request.OperationID)
			require.NoError(t, readErr)
			require.Equal(t, request.OperationID, committed.OperationID)
			require.True(t, client.IsResponseDecodeError(err), "committed mutation must have unknown outcome: %v", err)
			if failure.name == "byte_limit" {
				require.ErrorIs(t, err, client.ErrIntegrity)
			}
		})
	}
	require.NoError(t, c.RemoveEmailDocumentPublication(t.Context(), receipt.OperationID, receipt.RequestDigest))
	_, err = s.ContentVersionByID(t.Context(), receipt.Relations[0].Child.VersionID)
	require.NoError(t, err)
}
