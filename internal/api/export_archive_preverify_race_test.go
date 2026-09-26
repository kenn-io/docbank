package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/exporter"
	"go.kenn.io/docbank/internal/store"
)

// RED: the first visibility read may pass, then verification may take a long
// time. A source withdrawn during that interval must be denied before 200 or
// any ZIP bytes are written. The blocking lease models that interval without
// a large or timing-dependent archive fixture.
func TestNativeExportArchiveReadRechecksVisibilityAfterPreverify(t *testing.T) {
	root := t.TempDir()
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobsDir := filepath.Join(root, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	blobs, err := blob.New(store.NewPackCatalog(catalog), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	worker, err := exporter.New(catalog, blobs, root, NewOperationGate())
	require.NoError(t, err)
	hash, size, err := blobs.Write(bytes.NewReader([]byte("synthetic preverify race\n")))
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "race.txt", hash, size, "text/plain")
	require.NoError(t, err)
	source, err := catalog.CreateExportSource(t.Context(), "master", bundle.SourceRequest{
		OperationID: uuid.NewString(), Kind: "explicit",
		Members: []bundle.Member{{NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: hash, Size: size}},
	}, nil)
	require.NoError(t, err)
	plan, err := catalog.CreateExportPlan(t.Context(), "master", bundle.PlanRequest{
		OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash,
		Roles: []bundle.RolePolicy{{Role: "original"}},
	})
	require.NoError(t, err)
	job, err := catalog.QueueExportJob(t.Context(), "master", bundle.JobRequest{
		OperationID: uuid.NewString(), PlanID: plan.ID, Fingerprint: plan.Fingerprint,
	})
	require.NoError(t, err)
	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)

	verified, resume := make(chan struct{}), make(chan struct{})
	lease := func(ctx context.Context, owner, id string) (*os.File, bundle.Receipt, func(), error) {
		file, receipt, release, err := worker.Lease(ctx, owner, id)
		close(verified)
		select {
		case <-resume:
		case <-ctx.Done():
		}
		return file, receipt, release, err
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/exports/jobs/"+job.ID+"/archive", nil)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		serveExportArchiveRead(response, request, catalog, "master", job.ID, lease)
		close(done)
	}()
	select {
	case <-verified:
	case <-time.After(10 * time.Second):
		close(resume)
		t.Fatal("archive preverification did not start")
	}
	_, _, err = catalog.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	close(resume)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("archive read did not stop after source withdrawal")
	}
	require.Equal(t, http.StatusConflict, response.Code)
	require.Contains(t, response.Body.String(), `"code":"visibility_changed"`)
	require.NotEqual(t, "application/zip", response.Header().Get("Content-Type"))
}
