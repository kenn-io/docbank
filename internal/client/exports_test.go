package client_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/client"
)

func TestExportClientFrozenPreviewAndLocalNameValidation(t *testing.T) {
	c, s := newClient(t, serverKey)
	n, err := s.CreateFile(t.Context(), s.RootID(), "synthetic.txt", strings.Repeat("a", 64), 5, "text/plain")
	require.NoError(t, err)
	source, err := c.CreateExportSource(t.Context(), bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "explicit", Members: []bundle.Member{{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size}}})
	require.NoError(t, err)
	plan, err := c.CreateExportPlan(t.Context(), bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}, {Role: "text", AllowUnavailable: true}}})
	require.NoError(t, err)
	preview, err := c.ExportPlanPreview(t.Context(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, plan.Fingerprint, preview.Fingerprint)
	require.Equal(t, 1, preview.Roles[0].Files)
	require.Equal(t, int64(5), preview.Roles[0].Bytes)
	require.Equal(t, 1, preview.Roles[1].UnavailableMembers)

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	t.Cleanup(server.Close)
	_, err = client.New(server.URL, "synthetic-key").ExportDownloadTicketNamed(t.Context(), uuid.NewString(), bundle.DownloadRequest{Basename: "../unsafe.zip"})
	require.Error(t, err)
	require.Zero(t, requests)
}
