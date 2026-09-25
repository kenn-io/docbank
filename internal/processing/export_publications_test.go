package processing

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
)

func TestExportPublicationChoicesIncludeEmptySetsAndPage(t *testing.T) {
	t.Parallel()
	f := newEmailPipelineFixture(t)
	target := f.add(t, "empty.eml", "Subject: Empty\r\nContent-Type: text/plain\r\n\r\nNo attachments.\r\n", "message/rfc822")
	view, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	source, err := f.catalog.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "nodes", NodeIDs: []int64{target.Version.NodeID}}, nil)
	require.NoError(t, err)
	for i := range 51 {
		_, err = PublishEmailDocuments(t.Context(), f.catalog, f.blobs, pipelineDocumentRequest(t, f, view, fmt.Sprintf("empty-%02d", i)))
		require.NoError(t, err)
	}
	page, err := f.catalog.ExportAttachmentPublications(t.Context(), "owner", source.ID, 0)
	require.NoError(t, err)
	require.Equal(t, source.MemberHash, page.MemberHash)
	require.Equal(t, 51, page.Total)
	require.Equal(t, 50, page.Next)
	require.Len(t, page.Items, 50)
	require.Equal(t, "empty-00", page.Items[0].OperationID)
	require.Equal(t, "empty.eml", page.Items[0].Name)
	require.Zero(t, page.Items[0].Attachments)
	page, err = f.catalog.ExportAttachmentPublications(t.Context(), "owner", source.ID, page.Next)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Zero(t, page.Next)
	choice := page.Items[0]
	require.Equal(t, "empty-50", choice.OperationID)
	plan, err := f.catalog.CreateExportPlan(t.Context(), "owner", bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "attachment_original"}}, Publications: []bundle.PublicationSelection{{VersionID: choice.VersionID, OperationID: choice.OperationID}}})
	require.NoError(t, err)
	require.NoError(t, f.catalog.WalkExportDocuments(t.Context(), plan.ID, func(d bundle.Document) error {
		require.Equal(t, choice.OperationID, d.Inventory.OperationID)
		require.Equal(t, "complete", d.Inventory.State)
		return nil
	}))
}
