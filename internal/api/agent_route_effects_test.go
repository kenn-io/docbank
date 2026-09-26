package api_test

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/mailbox"
	"go.kenn.io/docbank/internal/store"
)

func TestAgentPageJobStatusPostOnlyReadsQueuedJob(t *testing.T) {
	ts, s := newTestServer(t, nil)
	node := createFileWithContent(t, ts, s, "/synthetic-page-source.txt", "synthetic source")
	selection := store.PageBinding{NodeID: node.ID, Revision: node.Revision,
		Source: document.PageSource{VersionID: node.CurrentVersionID, SHA256: node.BlobHash, Size: node.Size}}
	queued, err := s.QueuePageJob(t.Context(), uuid.NewString(), store.PageJobRequest{
		NodeID: node.ID, Revision: node.Revision, Source: selection.Source, Pages: []int{1},
		DPI: 72, RuntimeFingerprint: testHash("synthetic-page-runtime"),
	})
	require.NoError(t, err)

	response, body := do(t, ts, http.MethodPost, "/api/v1/pages/jobs/"+queued.ID, nil,
		api.PageSelectionRequest{Selection: selection})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var read store.PageRenderJob
	require.NoError(t, json.Unmarshal([]byte(body), &read))
	require.Equal(t, queued, read)
	after, err := s.PageJob(t.Context(), queued.ID, selection)
	require.NoError(t, err)
	require.Equal(t, queued, after, "status read must not change the queued job")
}

func TestAgentMailboxPreviewPostLeavesSealedContainerUnchanged(t *testing.T) {
	ts, s := newTestServer(t, nil)
	owner := "vault:" + s.VaultID()
	raw := []byte("From sender@example.test Sat Sep 12 10:00:00 2026\nSubject: Synthetic\n\nBody\n")
	hash := testHash(string(raw))
	service := mailbox.Service{Store: s.Store, Blobs: s.Blobs,
		Spool: filepath.Join(filepath.Dir(s.DBPath), "blobs", "tmp")}
	_, err := s.BeginMailboxContainer(t.Context(), store.MailboxContainerRequest{
		ID: "synthetic-preview", Owner: owner, SHA256: hash, Size: int64(len(raw)), Format: "mbox",
	})
	require.NoError(t, err)
	require.NoError(t, service.UploadChunk(t.Context(), owner, "synthetic-preview", 0,
		hash, int64(len(raw)), bytes.NewReader(raw)))
	before, err := service.Seal(t.Context(), owner, "synthetic-preview")
	require.NoError(t, err)

	response, body := do(t, ts, http.MethodPost,
		"/api/v1/mailbox/containers/synthetic-preview/preview", nil,
		map[string]string{"dialect": "mboxrd"})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var preview mailbox.Preview
	require.NoError(t, json.Unmarshal([]byte(body), &preview))
	require.Equal(t, 1, preview.EntryCount)
	require.Len(t, preview.Samples, 1)
	after, err := s.MailboxContainer(t.Context(), owner, "synthetic-preview")
	require.NoError(t, err)
	require.Equal(t, before, after, "preview must not change container authority")
}

func TestAgentContentVerificationPostLeavesNodeRevisionUnchanged(t *testing.T) {
	ts, s := newTestServer(t, nil)
	before := createFileWithContent(t, ts, s, "/synthetic-evidence.txt", "synthetic evidence")
	_, etag := etagOf(t, ts, before.ID)
	response, body := do(t, ts, http.MethodPost,
		fmt.Sprintf("/api/v1/nodes/%d/verify", before.ID), map[string]string{"If-Match": etag}, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var report api.ContentVerification
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	require.True(t, report.Verified)
	require.Equal(t, before.Revision, report.Revision)
	after, err := s.NodeByID(t.Context(), before.ID)
	require.NoError(t, err)
	require.Equal(t, before, after, "verification must not change node authority")
}
