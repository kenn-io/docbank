package store

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/reporting"
	"go.kenn.io/docbank/report"
)

func TestTermReportFamilyLimitExcludesEmptyPublications(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	unrelated := createCollectionRun(t, s, "alpha.txt", "empty-publication-scope")
	f := newEmailSourceFixture(t, s, "empty.eml", "Content-Type: text/plain\r\n\r\nSynthetic body\r\n")
	source, err := s.ContentVersionByID(t.Context(), f.publication.ContentVersionID)
	require.NoError(t, err)
	selected, err := s.BeginIngest(t.Context(), "cli", "Synthetic email collection")
	require.NoError(t, err)
	parent, err := s.IngestFileExact(t.Context(), selected, s.RootID(), "selected.eml", source.BlobHash,
		source.Size, "message/rfc822", "selected.eml", "")
	require.NoError(t, err)
	f.publication.ContentVersionID = parent.CurrentVersionID
	view, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	request := attachmentRequest(t, s, view, "empty-publication-0")
	receipt, err := s.PublishEmailDocuments(t.Context(), request)
	require.NoError(t, err)
	require.Empty(t, receipt.Relations)
	repeatTermReportPublication(t, s, request, receipt)
	for _, tc := range []struct{ name, collectionID string }{
		{"unrelated collection", unrelated.ID()},
		{"selected parent", selected.ID()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget := report.NewBudget(128 << 20)
			defer func() { _ = budget.Close() }()
			request := termFrameRequest()
			request.AllDocuments, request.CollectionIDs = false, []string{tc.collectionID}
			service := reporting.Service{Source: s, Budget: budget}
			frame, err := service.Prepare(t.Context(), request)
			require.NoError(t, err)
			require.Len(t, frame.Members, 1)
			require.Empty(t, frame.Relations)
		})
	}
}

func TestTermReportFamilyLimitCountsOnlyConnectedRelations(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	selected := createCollectionRun(t, s, "alpha.txt", "relation-scope")
	f := newEmailFixture(t, s, "external-parent.eml")
	view, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	request := attachmentRequest(t, s, view, "related-publication")
	receipt, err := s.PublishEmailDocuments(t.Context(), request)
	require.NoError(t, err)
	child, err := s.NodeByID(t.Context(), receipt.Relations[0].Child.NodeID)
	require.NoError(t, err)
	request = attachmentRequest(t, s, view, "related-publication")
	request.Reuse = []document.EmailDocumentReuse{{PartPath: receipt.Relations[0].PartPath,
		Child: *receipt.Relations[0].Child, Revision: child.Revision}}
	repeatTermReportPublication(t, s, request, receipt)
	budget := report.NewBudget(128 << 20)
	defer func() { _ = budget.Close() }()
	service := reporting.Service{Source: s, Budget: budget}
	reportRequest := termFrameRequest()
	reportRequest.AllDocuments, reportRequest.CollectionIDs = false, []string{selected.ID()}
	frame, err := service.Prepare(t.Context(), reportRequest)
	require.NoError(t, err)
	require.Len(t, frame.Members, 1)
	require.Empty(t, frame.Relations)
	_, err = service.Prepare(t.Context(), termFrameRequest())
	require.ErrorIs(t, err, report.ErrReportLimit, "connected actual relations still enforce the limit")
	require.ErrorContains(t, err, "family relation limit exceeded")
}

func repeatTermReportPublication(t *testing.T, s *Store, request document.EmailDocumentPublicationRequest,
	receipt document.EmailDocumentPublicationReceipt,
) {
	t.Helper()
	// Keep every receipt valid while avoiding a durable commit per operation.
	prefix := request.OperationID
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		for i := 1; i <= 100000; i++ {
			request.OperationID = fmt.Sprintf("%s-%d", prefix, i)
			receipt.OperationID = request.OperationID
			var err error
			receipt.RequestDigest, err = document.EmailDocumentRequestDigest(request)
			if err != nil {
				return err
			}
			for j := range receipt.Relations {
				receipt.Relations[j].OperationID = request.OperationID
			}
			if err := insertEmailDocumentPublication(t.Context(), tx, request, receipt); err != nil {
				return err
			}
		}
		return nil
	}))
	_, err := s.EmailDocumentPublication(t.Context(), request.OperationID)
	require.NoError(t, err)
}

func TestTermReportFamilyFollowsSelectedChildToCurrentExternalParents(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	f := newEmailFixture(t, s, "external-parent.eml")
	view, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	part := document.EmailAttachmentParts(view.Evidence)[0]
	selected, err := s.BeginIngest(t.Context(), "cli", "Synthetic selected attachment")
	require.NoError(t, err)
	child, err := s.IngestFileExact(t.Context(), selected, s.RootID(), "selected.bin",
		part.Payload.SHA256, part.Payload.Size, "application/octet-stream", "selected.bin", "")
	require.NoError(t, err)
	childVersion, err := s.ContentVersionByID(t.Context(), child.CurrentVersionID)
	require.NoError(t, err)
	for i := range 2 {
		if i != 0 {
			parent, err := s.CreateFile(t.Context(), s.RootID(), "other-external-parent.eml", view.Version.BlobHash,
				view.Version.Size, "message/rfc822")
			require.NoError(t, err)
			f.publication.ContentVersionID = parent.CurrentVersionID
			view, err = s.PublishEmailGeneration(t.Context(), f.publication)
			require.NoError(t, err)
		}
		request := attachmentRequest(t, s, view, fmt.Sprintf("shared-parent-%d", i))
		request.Reuse = []document.EmailDocumentReuse{{PartPath: part.Path, Child: documentIdentity(childVersion), Revision: child.Revision}}
		_, err := s.PublishEmailDocuments(t.Context(), request)
		require.NoError(t, err)
	}
	_, err = s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, "external-sibling"))
	require.NoError(t, err)
	request := termFrameRequest()
	request.AllDocuments, request.CollectionIDs = false, []string{selected.ID()}
	budget := report.NewBudget(8 << 20)
	defer func() { _ = budget.Close() }()
	service := reporting.Service{Source: s, Budget: budget}
	frame, err := service.Prepare(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, frame.Members, 1)
	require.Len(t, frame.Relations, 3)
	require.Len(t, frame.Coverage.Warnings, 1)
	parent, err := s.NodeByID(t.Context(), view.Version.NodeID)
	require.NoError(t, err)
	_, _, err = s.ReplaceContent(t.Context(), parent.ID, parent.Revision, fakeHash("replaced-external-parent"), 3, "text/plain")
	require.NoError(t, err)
	fresh, err := service.Prepare(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, fresh.Relations, 1)
	require.Empty(t, fresh.Coverage.Warnings)
}
