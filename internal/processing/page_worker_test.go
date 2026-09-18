package processing_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func pageWorkerRuntime(t *testing.T) *pagerender.Runtime {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("page runtime is Linux amd64 only")
	}
	renderer, err := exec.LookPath("pdftoppm")
	if err != nil {
		t.Skip("Poppler unavailable")
	}
	limiter, err := exec.LookPath("prlimit")
	if err != nil {
		t.Skip("prlimit unavailable")
	}
	inspector := filepath.Join(t.TempDir(), "page-inspect")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-tags", "fts5", "-o", inspector, "go.kenn.io/docbank/document/pagerender/cmd/docbank-page-inspect")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	pin := func(path string) pagerender.Executable {
		resolved, err := filepath.EvalSymlinks(path)
		require.NoError(t, err)
		data, err := os.ReadFile(resolved)
		require.NoError(t, err)
		hash := sha256.Sum256(data)
		return pagerender.Executable{Path: resolved, SHA256: hex.EncodeToString(hash[:])}
	}
	engine, err := pagerender.New(t.Context(), pagerender.Profile{Inspector: pin(inspector), Renderer: pin(renderer), Limiter: pin(limiter), DeploymentIdentity: "synthetic-worker-test"})
	require.NoError(t, err)
	return engine
}

func TestRealPageWorkerPublishesVerifiedImagesAndResumesExactJobs(t *testing.T) {
	engine := pageWorkerRuntime(t)
	root := t.TempDir()
	catalog, err := store.Open(filepath.Join(root, "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = catalog.Close() })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = blobs.Close() })
	data := mediatest.PDF()
	hash, size, err := blobs.Write(bytes.NewReader(data))
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "synthetic.pdf", hash, size, "application/pdf")
	require.NoError(t, err)
	request := store.PageJobRequest{NodeID: node.ID, Revision: node.Revision, Source: document.PageSource{VersionID: node.CurrentVersionID, SHA256: hash, Size: size}, Pages: []int{1}, DPI: 144, RuntimeFingerprint: engine.Fingerprint()}
	job, err := catalog.QueuePageJob(t.Context(), uuid.New().String(), request)
	require.NoError(t, err)
	gate := api.NewOperationGate()
	retried := map[string]bool{}
	worker, err := processing.NewPageWorker(catalog, blobs, engine, pageMutationFunc(func(ctx context.Context, fn func() error) error {
		current, err := catalog.PageJob(ctx, job.ID, request.Binding())
		if err != nil {
			return err
		}
		// Fail once before claiming and once after publication, before finishing.
		phase := ""
		if current.State == "queued" {
			phase = "claim"
		} else if current.State == "running" && len(current.Results) == 1 {
			phase = "finish"
		}
		if phase != "" && !retried[phase] {
			retried[phase] = true
			return sql.ErrConnDone
		}
		return gate.MutateContext(ctx, fn)
	}))
	require.NoError(t, err)
	processed, err := worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	require.Equal(t, map[string]bool{"claim": true, "finish": true}, retried)
	complete, err := catalog.PageJob(t.Context(), job.ID, request.Binding())
	require.NoError(t, err)
	require.Equal(t, "completed", complete.State)
	require.Len(t, complete.Results, 1)
	require.Equal(t, int64(32), complete.Results[0].Width)
	stream, err := blobs.OpenContext(t.Context(), complete.Results[0].SHA256)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = worker.RunOne(canceled)
	require.ErrorIs(t, err, context.Canceled)
}

type pageMutationFunc func(context.Context, func() error) error

func (f pageMutationFunc) MutateContext(ctx context.Context, fn func() error) error {
	return f(ctx, fn)
}
