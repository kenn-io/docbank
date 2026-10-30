package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/mistral/mistraltest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestWatchMistralImportConsentAndExactVersions(t *testing.T) {
	cfg, _ := mistralOCRProcessingConfig(t)
	profiles, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
	require.NoError(t, err)
	root, source := t.TempDir(), t.TempDir()
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, catalog.Close()) }()
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	defer func() { require.NoError(t, blobs.Close()) }()
	gate := api.NewOperationGate()
	service, err := processing.NewService(processing.ServiceConfig{Catalog: catalog, Blobs: blobs, Gate: gate,
		Profiles: profiles, SpoolDirectory: t.TempDir()})
	require.NoError(t, err)
	defer func() { require.NoError(t, service.Shutdown(context.Background())) }()
	watch := config.WatchConfig{Name: "inbox", Source: source, Destination: "/inbox", Profile: "documents",
		CaptureOnChange: true, ScanInterval: config.Duration(10 * time.Millisecond)}
	watcher, err := ingest.NewWatcher(&ingest.Ingester{Store: catalog, Blobs: blobs}, root, watch, gate.Mutate, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	type admission struct {
		node store.Node
		err  error
	}
	imports := make(chan admission, 8)
	watcher.AfterImport = func(ctx context.Context, node store.Node) error {
		err := service.AdmitImportedRendition(ctx, node, watch.Profile)
		imports <- admission{node, err}
		return err
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()
	defer func() { cancel(); <-done }()
	importFile := func(name string, content []byte) store.Node {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(source, name), content, 0600))
		select {
		case result := <-imports:
			require.NoError(t, result.err, "consent and unsupported-source skips complete quietly")
			return result.node
		case <-time.After(10 * time.Second):
			t.Fatal("watch import did not finish")
			return store.Node{}
		}
	}
	first := importFile("report.pdf", mistraltest.MinimalPDF("without consent"))
	_, queued, err := catalog.ClaimNextRenditionJob(t.Context(), "test-worker", time.Now(), time.Minute)
	require.NoError(t, err)
	require.False(t, queued)
	selector := processing.Selector{NodeID: first.ID, ContentVersionID: first.CurrentVersionID, Profile: "documents"}
	plan, err := service.Plan(t.Context(), selector)
	require.NoError(t, err)
	_, err = service.GrantConsent(t.Context(), processing.ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	missing := first
	missing.CurrentVersionID = "00000000-0000-4000-8000-000000000001"
	require.NoError(t, service.AdmitImportedRendition(t.Context(), missing, "documents"))
	updated := importFile("report.pdf", mistraltest.MinimalPDF("with consent"))
	require.NotEqual(t, first.CurrentVersionID, updated.CurrentVersionID)
	require.NoError(t, service.AdmitImportedRendition(t.Context(), first, "documents"), "superseded authority finishes quietly")
	job, queued, err := catalog.ClaimNextRenditionJob(t.Context(), "test-worker", time.Now(), time.Minute)
	require.NoError(t, err)
	require.True(t, queued)
	work, err := catalog.RenditionJobWorkByClaim(t.Context(), job, time.Now())
	require.NoError(t, err)
	require.Equal(t, updated.CurrentVersionID, work.Waiter.ContentVersionID)
	importFile("notes.txt", []byte("synthetic text"))
	_, err = service.RevokeConsent(t.Context())
	require.NoError(t, err)
	importFile("next.pdf", mistraltest.MinimalPDF("after revocation"))
	_, queued, err = catalog.ClaimNextRenditionJob(t.Context(), "test-worker", time.Now(), time.Minute)
	require.NoError(t, err)
	require.False(t, queued)
}

func TestDaemonRejectsWatchProfileWithoutRuntime(t *testing.T) {
	cfg, _ := mistralOCRProcessingConfig(t)
	profile := cfg.RenditionProfiles["documents"]
	profile.Runtime = nil
	cfg.RenditionProfiles["documents"] = profile
	cfg.Watches = []config.WatchConfig{{Name: "inbox", Source: t.TempDir(), Destination: "/inbox", Profile: "documents", CaptureOnChange: true, SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second)}}
	require.NoError(t, cfg.Validate())
	root := t.TempDir()
	require.NoError(t, writeDaemonASRConfig(root, cfg))
	t.Setenv("DOCBANK_HOME", root)
	require.ErrorContains(t, runServe(t.Context()), `watch "inbox" profile "documents" has no executable runtime`)
}
