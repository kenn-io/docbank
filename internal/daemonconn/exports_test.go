package daemonconn_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/apiclient"
	"uuid"
)

func TestExportClientFrozenPreview(t *testing.T) {
	c, s := newClient(t, serverKey)
	n, err := s.CreateFile(t.Context(), s.RootID(), "synthetic.txt", strings.Repeat("a", 64), 5, "text/plain")
	require.NoError(t, err)
	source, err := c.API().CreateExportSource(t.Context(), &apiclient.CreateExportSourceRequestOptions{Body: &bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size}}}})
	require.NoError(t, err)
	plan, err := c.API().CreateExportPlan(t.Context(), &apiclient.CreateExportPlanRequestOptions{Body: &bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}, {Role: "text", AllowUnavailable: true}}}})
	require.NoError(t, err)
	preview, err := c.API().GetExportPlanPreview(t.Context(), &apiclient.GetExportPlanPreviewRequestOptions{PathParams: &apiclient.GetExportPlanPreviewPath{ID: plan.ID}})
	require.NoError(t, err)
	require.Equal(t, plan.Fingerprint, preview.Fingerprint)
	require.Equal(t, 1, preview.Roles[0].Files)
	require.Equal(t, int64(5), preview.Roles[0].Bytes)
	require.Equal(t, 1, preview.Roles[1].UnavailableMembers)
}
