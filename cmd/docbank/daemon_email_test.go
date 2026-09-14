package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestStartProcessingJobsRegistersEmailWithoutRenditionRuntime(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	layout := home.Layout{Root: root}
	require.NoError(t, layout.Ensure())
	catalog, err := store.Open(layout.DBPath())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), layout.BlobsDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	ctx, cancel := context.WithCancel(context.Background())
	supervisor := jobs.New(ctx, slog.New(slog.DiscardHandler))
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		require.NoError(t, supervisor.Shutdown(shutdownCtx))
		cancel()
	})
	err = startProcessingJobs(
		supervisor, catalog, blobs, layout.BlobTmpDir(),
		processing.NewRenditionRuntimeRegistry(), api.NewOperationGate(), slog.Default(),
	)
	require.NoError(t, err)
	var names []string
	for _, job := range supervisor.Snapshot() {
		names = append(names, job.Name)
	}
	require.Contains(t, names, "extract:email")
	require.NotContains(t, names, "process:renditions")
}

func TestDaemonProcessesEmailWithoutExternalRenditionProvider(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "message.eml", "Content-Type: text/plain; charset=utf-8\r\n\r\ndaemonemailmarker")
	_, err := runCLI(t, "add", source, "--dest", "/mail")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		out, searchErr := runCLI(t, "search", "daemonemailmarker")
		return searchErr == nil && strings.Contains(out, "/mail/message.eml")
	}, 15*time.Second, 25*time.Millisecond)
}

func TestServeRecoversAbandonedEmailSpoolBeforeBlobCleanup(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	layout := home.Layout{Root: root}
	require.NoError(t, layout.Ensure())
	spool := filepath.Join(layout.BlobTmpDir(), "docbank-email-abandoned")
	require.NoError(t, os.Mkdir(spool, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(spool, ".docbank-email-spool"), []byte("docbank-email-spool/v1\n"), 0o600,
	))
	require.NoError(t, os.WriteFile(filepath.Join(spool, "source"), []byte("abandoned"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(layout.BlobTmpDir(), "blob-unfinished"), []byte("partial"), 0o600))

	t.Setenv("DOCBANK_HOME", root)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			require.FailNow(t, "daemon did not stop after email spool recovery")
		}
	})
	require.Eventually(t, func() bool {
		_, _, ok, findErr := client.Find(ctx, root)
		return findErr == nil && ok
	}, 30*time.Second, 25*time.Millisecond)
	entries, err := os.ReadDir(layout.BlobTmpDir())
	require.NoError(t, err)
	require.Empty(t, entries)
}
