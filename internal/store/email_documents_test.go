package store

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"testing"
)

func attachmentRequest(t *testing.T, s *Store, view EmailMetadataView, operation string) document.EmailDocumentPublicationRequest {
	t.Helper()
	dir, err := s.NodeByID(t.Context(), s.RootID())
	require.NoError(t, err)
	return document.EmailDocumentPublicationRequest{OperationID: operation, Parent: document.EmailDocumentIdentity{NodeID: view.Version.NodeID, VersionID: view.Version.ID, SHA256: view.Version.BlobHash, Size: view.Version.Size}, GenerationID: view.Generation.ID, AttachmentID: view.Attachment.ID, DestinationID: dir.ID, DestinationRevision: dir.Revision, Reuse: []document.EmailDocumentReuse{}}
}

// Losing the response must not duplicate children, and changing retry input
// must never reinterpret the original operation.
func TestEmailDocumentsPublicationRetryAndExactOccurrence(t *testing.T) {
	s := newTestStore(t)
	f := newEmailFixture(t, s, "source.eml")
	view, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	req := attachmentRequest(t, s, view, "mail-publication-1")
	receipt, err := s.PublishEmailDocuments(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "complete", receipt.InventoryState)
	require.Len(t, receipt.Relations, 1)
	rel := receipt.Relations[0]
	require.Equal(t, "1.2", rel.PartPath)
	require.Equal(t, req.Parent, rel.Parent)
	require.NotNil(t, rel.Child)
	require.Equal(t, int64(4), rel.Child.Size)
	require.Equal(t, "decoded", rel.Outcome)
	require.NotEqual(t, rel.Parent.NodeID, rel.Child.NodeID)
	again, err := s.PublishEmailDocuments(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, receipt, again)
	req.DestinationRevision++
	_, err = s.PublishEmailDocuments(t.Context(), req)
	require.ErrorIs(t, err, ErrEmailDocumentConflict)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestEmailDocumentsAtomicStaleDestinationAndExplicitReuse(t *testing.T) {
	s := newTestStore(t)
	f := newEmailFixture(t, s, "source.eml")
	view, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	req := attachmentRequest(t, s, view, "mail-stale")
	req.DestinationRevision++
	_, err = s.PublishEmailDocuments(t.Context(), req)
	require.ErrorIs(t, err, ErrStaleRevision)
	var count int
	require.NoError(t, s.db.QueryRow("SELECT count(*) FROM email_document_publications").Scan(&count))
	require.Zero(t, count)
	req = attachmentRequest(t, s, view, "mail-first")
	first, err := s.PublishEmailDocuments(t.Context(), req)
	require.NoError(t, err)
	req = attachmentRequest(t, s, view, "mail-second")
	second, err := s.PublishEmailDocuments(t.Context(), req)
	require.NoError(t, err)
	require.NotEqual(t, first.Relations[0].Child.NodeID, second.Relations[0].Child.NodeID)
	require.Equal(t, first.Relations[0].Child.SHA256, second.Relations[0].Child.SHA256)
	child, err := s.NodeByID(t.Context(), first.Relations[0].Child.NodeID)
	require.NoError(t, err)
	other, err := s.CreateFile(t.Context(), s.RootID(), "other-parent.eml", view.Version.BlobHash, view.Version.Size, "message/rfc822")
	require.NoError(t, err)
	otherPublication := f.publication
	otherPublication.ContentVersionID = other.CurrentVersionID
	otherView, err := s.PublishEmailGeneration(t.Context(), otherPublication)
	require.NoError(t, err)
	req = attachmentRequest(t, s, otherView, "mail-reuse")
	req.Reuse = []document.EmailDocumentReuse{{PartPath: "1.2", Child: *first.Relations[0].Child, Revision: child.Revision}}
	reused, err := s.PublishEmailDocuments(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, first.Relations[0].Child, reused.Relations[0].Child)
	page, err := s.EmailDocumentRelations(t.Context(), document.EmailDocumentRelationQuery{ChildVersionID: child.CurrentVersionID})
	require.NoError(t, err)
	require.Equal(t, int64(2), page.Total)
	require.NotEqual(t, page.Items[0].Relation.Parent.VersionID, page.Items[1].Relation.Parent.VersionID)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}
