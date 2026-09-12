package client_test

import (
	"crypto/sha256"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"strings"
	"testing"
)

func TestClientEmailDocumentsExplicitConsent(t *testing.T) {
	c, s := newClient(t, serverKey)
	r := document.ProcessingConsentRequest{Principal: "operator:synthetic", Scope: "attachment:synthetic", ProfileFingerprint: fmtHash(sha256.Sum256([]byte("profile"))), DisclosureFingerprint: fmtHash(sha256.Sum256([]byte("disclosure"))), InputClasses: []string{"original_source"}}
	a := store.ProviderOperationAuthorizationRequest{Principal: r.Principal, Scope: r.Scope, ProfileFingerprint: r.ProfileFingerprint, DisclosureFingerprint: r.DisclosureFingerprint, InputClasses: r.InputClasses}
	_, err := s.AuthorizeProviderOperation(t.Context(), a)
	require.ErrorIs(t, err, store.ErrProcessingConsentRequired)
	grant, err := c.GrantProcessingConsent(t.Context(), r)
	require.NoError(t, err)
	authorized, err := s.AuthorizeProviderOperation(t.Context(), a)
	require.NoError(t, err)
	require.Equal(t, grant.GrantID, authorized.GrantID)
	revoked, err := c.RevokeProcessingConsent(t.Context(), document.ProcessingConsentRevocationRequest{Principal: r.Principal, Scope: r.Scope})
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
	require.NoError(t, c.RemoveEmailDocumentPublication(t.Context(), receipt.OperationID, receipt.RequestDigest))
	_, err = s.ContentVersionByID(t.Context(), receipt.Relations[0].Child.VersionID)
	require.NoError(t, err)
}
