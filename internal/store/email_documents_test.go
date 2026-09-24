package store

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"strings"
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
	t.Parallel()
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
	t.Parallel()
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

func TestEmailDocumentsEncryptedAncestry(t *testing.T) {
	t.Parallel()
	const encrypted = "Content-Type: multipart/encrypted; boundary=encrypted; protocol=\"application/pgp-encrypted\"\n\n" +
		"--encrypted\nContent-Type: application/pgp-encrypted\n\nVersion: 1\n" +
		"--encrypted\nContent-Type: application/octet-stream\nContent-Disposition: attachment; filename=encrypted.asc\n\nSynthetic ciphertext\n" +
		"--encrypted--\n"
	const nested = "Content-Type: multipart/mixed; boundary=nested\n\n" +
		"--nested\nContent-Type: text/plain\n\nSynthetic body\n" +
		"--nested\n" + encrypted +
		"--nested\nContent-Type: message/rfc822\nContent-Disposition: attachment; filename=forwarded.eml\n\n" + encrypted +
		"--nested\nContent-Type: application/pdf\nContent-Disposition: attachment; filename=plain.pdf\n\n%PDF-1.4\n" +
		"--nested--\n"
	for _, tc := range []struct {
		name     string
		source   string
		outcomes map[string]string
	}{
		{"root", encrypted, map[string]string{"1.1": "encrypted", "1.2": "encrypted"}},
		{"nested", nested, map[string]string{"1.2.1": "encrypted", "1.2.2": "encrypted", "1.3": "decoded", "1.4": "decoded"}},
		{"encrypted attached message", "Content-Type: multipart/encrypted; boundary=outer\n\n--outer\n" + nested + "--outer--\n", map[string]string{
			"1.1.1": "encrypted", "1.1.2.1": "encrypted", "1.1.2.2": "encrypted", "1.1.3": "encrypted", "1.1.4": "encrypted",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			f := newEmailSourceFixture(t, s, "encrypted.eml", strings.ReplaceAll(tc.source, "\n", "\r\n"))
			view, err := s.PublishEmailGeneration(t.Context(), f.publication)
			require.NoError(t, err)
			receipt, err := s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, "encrypted-publication"))
			require.NoError(t, err)
			require.Len(t, receipt.Relations, len(tc.outcomes))
			for _, rel := range receipt.Relations {
				require.Equal(t, tc.outcomes[rel.PartPath], rel.Outcome, rel.PartPath)
				if rel.Outcome == "encrypted" {
					require.Nil(t, rel.Child, rel.PartPath)
				} else {
					require.NotNil(t, rel.Child, rel.PartPath)
				}
			}
			document.EmailAttachmentParts(view.Evidence)
			for _, part := range view.Evidence.Inventory.Parts {
				if *part.Media.Declared != "multipart/encrypted" {
					require.Equal(t, document.EmailProtectionNone, part.Protection, "retained MIME protection changed at %s", part.Path)
				}
			}
			require.NoError(t, s.ValidateMetadata(t.Context()))
		})
	}
}

func TestEmailDocumentsExplicitRootAttachment(t *testing.T) {
	t.Parallel()
	const payload = "%PDF-1.4\r\nSynthetic attachment\r\n%%EOF\r\n"
	for _, explicit := range []bool{false, true} {
		name := "body"
		raw := "Content-Type: text/plain\r\n\r\nSynthetic body\r\n"
		if explicit {
			name = "attachment"
			raw = "Content-Type: application/pdf\r\nContent-Disposition: attachment; filename=root.pdf\r\n\r\n" + payload
		}
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			f := newEmailSourceFixture(t, s, "root.eml", raw)
			view, err := s.PublishEmailGeneration(t.Context(), f.publication)
			require.NoError(t, err)
			receipt, err := s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, "root-publication"))
			require.NoError(t, err)
			if !explicit {
				require.Empty(t, receipt.Relations)
				return
			}
			require.Len(t, receipt.Relations, 1)
			relation := receipt.Relations[0]
			require.Equal(t, "1", relation.PartPath)
			require.Equal(t, "decoded", relation.Outcome)
			require.NotNil(t, relation.Child)
			require.Equal(t, []byte(payload), f.bytes[relation.Child.SHA256])
			require.Equal(t, int64(len(payload)), relation.Child.Size)
			require.NotEqual(t, view.Version.BlobHash, relation.Child.SHA256)
			require.NoError(t, s.ValidateMetadata(t.Context()))
		})
	}
}
