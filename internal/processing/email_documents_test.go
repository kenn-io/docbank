package processing

import (
	"context"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"testing"
)

func pipelineDocumentRequest(t *testing.T, f emailPipelineFixture, view store.EmailMetadataView, id string) document.EmailDocumentPublicationRequest {
	t.Helper()
	dir, err := f.catalog.NodeByID(t.Context(), f.catalog.RootID())
	require.NoError(t, err)
	return document.EmailDocumentPublicationRequest{OperationID: id, Parent: document.EmailDocumentIdentity{NodeID: view.Version.NodeID, VersionID: view.Version.ID, SHA256: view.Version.BlobHash, Size: view.Version.Size}, GenerationID: view.Generation.ID, AttachmentID: view.Attachment.ID, DestinationID: dir.ID, DestinationRevision: dir.Revision, Reuse: []document.EmailDocumentReuse{}}
}
func TestEmailDocumentsVerifiedPublicationAndCanceledAttempt(t *testing.T) {
	f := newEmailPipelineFixture(t)
	target := f.add(t, "source.eml", emailPipelineSource, "message/rfc822")
	view, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	request := pipelineDocumentRequest(t, f, view, "verified-parts")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = PublishEmailDocuments(ctx, f.catalog, f.blobs, request)
	require.ErrorIs(t, err, context.Canceled)
	_, err = f.catalog.EmailDocumentPublication(t.Context(), request.OperationID)
	require.ErrorIs(t, err, store.ErrNotFound)
	receipt, err := PublishEmailDocuments(t.Context(), f.catalog, f.blobs, request)
	require.NoError(t, err)
	require.Len(t, receipt.Relations, 2)
	require.Equal(t, "1.2", receipt.Relations[0].PartPath)
	require.Equal(t, "1.3", receipt.Relations[1].PartPath)
	require.NotNil(t, receipt.Relations[0].Child)
	again, err := PublishEmailDocuments(t.Context(), f.catalog, f.blobs, request)
	require.NoError(t, err)
	require.Equal(t, receipt, again)
	page, err := f.catalog.EmailDocumentRelations(t.Context(), document.EmailDocumentRelationQuery{ChildVersionID: receipt.Relations[0].Child.VersionID})
	require.NoError(t, err)
	require.Equal(t, int64(1), page.Total)
	require.Equal(t, target.Version.ID, page.Items[0].Relation.Parent.VersionID)
	hits, _, err := f.catalog.SearchPage(t.Context(), "attachmentonlymarker", 10)
	require.NoError(t, err)
	require.Empty(t, hits)
	f.emptySpool(t)
}
