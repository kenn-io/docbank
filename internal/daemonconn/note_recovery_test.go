package daemonconn_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestNotePublicationAcceptLocalClient(t *testing.T) {
	t.Parallel()
	c, s := newClient(t, serverKey)
	scan, err := s.BeginNotePublicationScan(t.Context(), store.NotePublicationBeginRequest{
		Name: "research", Destination: "/notes", PolicyFingerprint: strings.Repeat("a", 64), OperationID: uuid.NewV4().String(),
	})
	require.NoError(t, err)
	receipt, err := s.AcceptPublishedNote(t.Context(), store.NotePublicationUploadRequest{
		Name: "research", ScanID: scan.ScanID, SourceRef: "alpha.md", FileName: "alpha.md", ParentPath: "/notes",
		OperationID: uuid.NewV4().String(), Hash: strings.Repeat("b", 64), Size: 4,
	}, store.BlobPhysical{Encoding: "raw", StoredBytes: 4, PackEligible: true, Created: true})
	require.NoError(t, err)
	node, err := s.NodeByID(t.Context(), receipt.Source.Identity.NodeID)
	require.NoError(t, err)
	_, version, err := s.ReplaceContent(t.Context(), node.ID, node.Revision, strings.Repeat("c", 64), 4, "text/markdown")
	require.NoError(t, err)
	request := api.NotePublicationAcceptLocalRequest{Name: "research", DatabaseEpoch: scan.DatabaseEpoch, SourceRef: "alpha.md",
		ExpectedHeadVersionID: version.ID, ExpectedSourceRevision: receipt.Source.Identity.SourceRevision, OperationID: uuid.NewV4().String()}
	grant, err := c.AcceptLocalPublishedNote(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, "alpha.md", grant.SourceRef)
	require.Equal(t, version.ID, grant.ExpectedHeadVersionID)
	require.Equal(t, request.ExpectedSourceRevision, grant.SourceRevision)
	after, err := s.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	require.Equal(t, version.ID, after.CurrentVersionID, "a grant authorizes a later upload without replacing the head")
	status, err := s.NotePublicationStatus(t.Context(), "research")
	require.NoError(t, err)
	require.EqualValues(t, 1, status.RecoveryPending)
}

// A response for another source, head or revision must never authorize the
// caller to proceed with a fresh local upload.
func TestNotePublicationAcceptLocalRejectsUnboundReceipt(t *testing.T) {
	t.Parallel()
	request := api.NotePublicationAcceptLocalRequest{
		Name: "research", DatabaseEpoch: "11111111-1111-4111-8111-111111111111", SourceRef: "alpha.md",
		ExpectedHeadVersionID: "22222222-2222-4222-8222-222222222222", ExpectedSourceRevision: 3,
		OperationID: "33333333-3333-4333-8333-333333333333",
	}
	for _, test := range []struct {
		name   string
		result api.NotePublicationRecoveryGrant
	}{
		{"source", api.NotePublicationRecoveryGrant{SourceRef: "other.md", ExpectedHeadVersionID: request.ExpectedHeadVersionID, SourceRevision: 3}},
		{"head", api.NotePublicationRecoveryGrant{SourceRef: "alpha.md", ExpectedHeadVersionID: "44444444-4444-4444-8444-444444444444", SourceRevision: 3}},
		{"revision", api.NotePublicationRecoveryGrant{SourceRef: "alpha.md", ExpectedHeadVersionID: request.ExpectedHeadVersionID, SourceRevision: 2}},
		{"empty", api.NotePublicationRecoveryGrant{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/note-publications/research/sources/accept-local", r.URL.Path)
				assert.Equal(t, serverKey, r.Header.Get("X-Api-Key"))
				w.Header().Set("Content-Type", "application/json")
				body, err := json.Marshal(test.result)
				if !assert.NoError(t, err) {
					return
				}
				_, _ = w.Write(body)
			}))
			defer ts.Close()
			c := daemonconn.New(ts.URL, serverKey)
			_, err := c.AcceptLocalPublishedNote(t.Context(), request)
			require.Error(t, err)
		})
	}
}
