package store

import (
	"bytes"
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"strings"
	"sync"
	"testing"
)

func TestEmailDocumentsMetadataRoundTripAndTamper(t *testing.T) {
	s := newTestStore(t)
	f := newEmailFixture(t, s, "source.eml")
	view, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	request := attachmentRequest(t, s, view, "portable-receipt")
	receipt, err := s.PublishEmailDocuments(t.Context(), request)
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &out))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(out.Bytes())))
	again, err := restored.PublishEmailDocuments(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, receipt, again)
	page, err := restored.EmailDocumentRelations(t.Context(), document.EmailDocumentRelationQuery{ChildVersionID: receipt.Relations[0].Child.VersionID})
	require.NoError(t, err)
	require.Equal(t, int64(1), page.Total)
	require.Equal(t, receipt.Relations[0], page.Items[0].Relation)
	var roundtrip bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &roundtrip))
	require.Equal(t, out.String(), roundtrip.String())
	for _, mutation := range []string{"child", "part", "outcome", "inventory", "request", "extra"} {
		t.Run(mutation, func(t *testing.T) {
			lines := bytes.Split(out.Bytes(), []byte("\n"))
			for i, line := range lines {
				if !bytes.Contains(line, []byte(`"type":"email_document_publication"`)) {
					continue
				}
				var record metadataEmailDocumentPublication
				require.NoError(t, json.Unmarshal(line, &record))
				switch mutation {
				case "child":
					record.Receipt.Relations[0].Child = &record.Request.Parent
				case "part":
					record.Receipt.Relations[0].PartPath = "1.1"
				case "outcome":
					record.Receipt.Relations[0].Outcome = "indexed"
				case "inventory":
					record.Receipt.InventoryState = "partial"
				case "request":
					record.Request.DestinationRevision++
				case "extra":
					record.Receipt.Relations = append(record.Receipt.Relations, record.Receipt.Relations[0])
				}
				encoded, err := json.Marshal(record)
				require.NoError(t, err)
				lines[i] = encoded
			}
			target := newTestStore(t)
			require.Error(t, target.ImportMetadata(t.Context(), bytes.NewReader(bytes.Join(lines, []byte("\n")))))
			var count int
			require.NoError(t, target.db.QueryRow(`SELECT count(*) FROM email_document_publications`).Scan(&count))
			require.Zero(t, count)
		})
	}
}

func TestEmailDocumentsDuplicateNamesBytesPartialAndAuditedCreation(t *testing.T) {
	s := newTestStore(t)
	raw := "Content-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: text/plain\r\n\r\nbody\r\n"
	part := "--m\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"../same.bin\"\r\n\r\nduplicate\r\n"
	raw += part + part + "--m--\r\n"
	f := newEmailSourceFixture(t, s, "source.eml", raw)
	view, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	plan, err := s.PreviewInitialAudit(t.Context(), s.RootID(), "api", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	receipt, err := s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, "audited"))
	require.NoError(t, err)
	require.Len(t, receipt.Relations, 2)
	a, b := receipt.Relations[0], receipt.Relations[1]
	require.Equal(t, a.Child.SHA256, b.Child.SHA256)
	require.NotEqual(t, a.Child.NodeID, b.Child.NodeID)
	for _, r := range receipt.Relations {
		node, err := s.NodeByID(t.Context(), r.Child.NodeID)
		require.NoError(t, err)
		require.NotContains(t, node.Name, "/")
		require.NotContains(t, node.Name, `\`)
	}
	require.NoError(t, s.ValidateMetadata(t.Context()))
	// Broken multipart termination is retained as partial, never promoted by
	// successfully publishing the parts that were decoded before the failure.
	partial := newEmailSourceFixture(t, s, "partial.eml", strings.TrimSuffix(raw, "--m--\r\n"))
	partialView, err := s.PublishEmailGeneration(t.Context(), partial.publication)
	require.NoError(t, err)
	partialReceipt, err := s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, partialView, "partial"))
	require.NoError(t, err)
	require.Equal(t, "partial", partialReceipt.InventoryState)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestEmailDocumentsRetainReferencesAcrossTrashReprocessPruneAndPurge(t *testing.T) {
	s := newTestStore(t)
	f := newEmailFixture(t, s, "source.eml")
	view, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	receipt, err := s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, "retained"))
	require.NoError(t, err)
	child := receipt.Relations[0].Child
	trashed, _, err := s.Trash(t.Context(), view.Version.NodeID, UnconditionalRev)
	require.NoError(t, err)
	node, err := s.NodeByID(t.Context(), child.NodeID)
	require.NoError(t, err)
	require.Nil(t, node.TrashedAt)
	_, err = s.TrashEmpty(t.Context(), 0, true)
	require.ErrorIs(t, err, ErrEmailDocumentConflict)
	_, _, err = s.Restore(t.Context(), trashed.ID, trashed.Revision)
	require.NoError(t, err)
	changed := view.Evidence
	changed.Recipe.GoVersion = "go1.27.99"
	pub := f.publication
	pub.CanonicalJSON, _, err = document.MarshalEmailV1(changed)
	require.NoError(t, err)
	newer, err := s.PublishEmailGeneration(t.Context(), pub)
	require.NoError(t, err)
	require.NotEqual(t, view.Generation.ID, newer.Generation.ID)
	head, _, err := s.ReplaceContent(t.Context(), view.Version.NodeID, UnconditionalRev, fakeHash("dd"), 3, "message/rfc822")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(t.Context(), head.ID, head.Revision, VersionPruneSelector{VersionIDs: []string{view.Version.ID}}, true)
	require.ErrorIs(t, err, ErrEmailDocumentConflict)
	_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{ContentVersionIDs: []string{view.Version.ID}})
	require.ErrorIs(t, err, ErrEmailDocumentConflict)
	page, err := s.EmailDocumentRelations(t.Context(), document.EmailDocumentRelationQuery{ChildVersionID: child.VersionID})
	require.NoError(t, err)
	require.Equal(t, view.Generation.ID, page.Items[0].Relation.GenerationID)
	require.Equal(t, view.Version.ID, page.Items[0].Relation.Parent.VersionID)
	childHead, _, err := s.ReplaceContent(t.Context(), child.NodeID, UnconditionalRev, fakeHash("de"), 4, "text/plain")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(t.Context(), childHead.ID, childHead.Revision, VersionPruneSelector{VersionIDs: []string{child.VersionID}}, true)
	require.ErrorIs(t, err, ErrEmailDocumentConflict)
	require.ErrorIs(t, s.RemoveEmailDocumentPublication(t.Context(), receipt.OperationID, fakeHash("ab")), ErrEmailDocumentConflict)
	require.NoError(t, s.RemoveEmailDocumentPublication(t.Context(), receipt.OperationID, receipt.RequestDigest))
	_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{ContentVersionIDs: []string{view.Version.ID}})
	require.NoError(t, err)
	_, err = s.ContentVersionByID(t.Context(), child.VersionID)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestEmailDocumentsConcurrentReplayBoundedPagesAndRollback(t *testing.T) {
	s := newTestStore(t)
	f := newEmailFixture(t, s, "source.eml")
	view, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	request := attachmentRequest(t, s, view, "concurrent")
	var wg sync.WaitGroup
	results := make([]document.EmailDocumentPublicationReceipt, 6)
	errs := make([]error, 6)
	for i := range results {
		wg.Go(func() { results[i], errs[i] = s.PublishEmailDocuments(t.Context(), request) })
	}
	wg.Wait()
	for i := range results {
		require.NoError(t, errs[i])
		require.Equal(t, results[0], results[i])
	}
	next := attachmentRequest(t, s, view, "second")
	_, err = s.PublishEmailDocuments(t.Context(), next)
	require.NoError(t, err)
	query := document.EmailDocumentRelationQuery{ParentVersionID: view.Version.ID, Limit: 1}
	first, err := s.EmailDocumentRelations(t.Context(), query)
	require.NoError(t, err)
	require.Equal(t, int64(2), first.Total)
	require.Len(t, first.Items, 1)
	require.NotEmpty(t, first.NextOperationID)
	query.AfterOperationID = first.NextOperationID
	query.AfterOrder = first.NextOrder
	last, err := s.EmailDocumentRelations(t.Context(), query)
	require.NoError(t, err)
	require.Len(t, last.Items, 1)
	require.Empty(t, last.NextOperationID)
	require.NotEqual(t, first.Items[0].Relation.OperationID, last.Items[0].Relation.OperationID)
	query.Limit = 251
	_, err = s.EmailDocumentRelations(t.Context(), query)
	require.ErrorIs(t, err, ErrInvalidEmailDocumentRequest)
	// Fail only after the ordinary child was inserted: all audit, node, version,
	// queue and relation writes must roll back with the failed receipt.
	_, err = s.db.Exec(`CREATE TRIGGER fail_email_documents BEFORE INSERT ON email_document_publications BEGIN SELECT RAISE(ABORT,'synthetic receipt failure'); END`)
	require.NoError(t, err)
	var before, after int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM nodes`).Scan(&before))
	next = attachmentRequest(t, s, view, "rollback")
	_, err = s.PublishEmailDocuments(t.Context(), next)
	require.ErrorContains(t, err, "synthetic receipt failure")
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM nodes`).Scan(&after))
	require.Equal(t, before, after)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestEmailDocumentsUnavailableOutcomesNeverCreateEmptyChildren(t *testing.T) {
	s := newTestStore(t)
	raw := "Content-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--m\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=unsupported.bin\r\nContent-Transfer-Encoding: unknown\r\n\r\nabc\r\n" +
		"--m\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=failed.bin\r\nContent-Transfer-Encoding: base64\r\n\r\n!!!\r\n" +
		"--m\r\nContent-Type: application/pkcs7-mime; smime-type=enveloped-data\r\nContent-Disposition: attachment; filename=encrypted.p7m\r\n\r\nabc\r\n--m--\r\n"
	f := newEmailSourceFixture(t, s, "source.eml", raw)
	view, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	receipt, err := s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, "unavailable"))
	require.NoError(t, err)
	require.Len(t, receipt.Relations, 3)
	for i, state := range []string{"unsupported", "failed", "encrypted"} {
		require.Equal(t, state, receipt.Relations[i].Outcome)
		require.Nil(t, receipt.Relations[i].Child)
	}
	require.NoError(t, s.ValidateMetadata(t.Context()))
}
