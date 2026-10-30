package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/mistral/mistraltest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/jobs"
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
	cfg.Watches = []config.WatchConfig{watch}
	cfg.Server.APIKey = "synthetic-key"
	supervisor := jobs.New(t.Context(), slog.New(slog.DiscardHandler))
	defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
	for _, name := range []string{"watch:inbox", "derive:visual-previews"} {
		require.NoError(t, supervisor.Start(name, func(ctx context.Context) error { <-ctx.Done(); return nil }))
	}
	server := api.NewServer(api.Deps{Store: catalog, Blobs: blobs, VaultRoot: root, Cfg: cfg, Processing: service, Jobs: supervisor})
	defer server.Close()
	endpoint := httptest.NewServer(server.Handler())
	defer endpoint.Close()
	client := daemonconn.New(endpoint.URL, cfg.Server.APIKey)
	defer func() { require.NoError(t, client.Close()) }()
	checkState := func(want string) {
		t.Helper()
		listed, err := client.API().ListWatchedInboxes(t.Context())
		require.NoError(t, err)
		require.Equal(t, "documents", listed.Items[0].Processing.Profile)
		require.Equal(t, want, listed.Items[0].Processing.ConsentState)
	}
	checkState("required")
	watcher, err := ingest.NewWatcher(&ingest.Ingester{Store: catalog, Blobs: blobs}, root, watch, gate.Mutate, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	imports := make(chan store.Node, 8)
	watcher.AfterImport = func(ctx context.Context, node store.Node) error {
		err := service.AdmitImportedRendition(ctx, node, watch.Profile)
		require.NoError(t, err, "consent and unsupported-source skips complete quietly")
		imports <- node
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
		case node := <-imports:
			return node
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
	expires := time.Now().Add(2 * time.Second)
	_, err = service.GrantConsent(t.Context(), processing.ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, ExpiresAt: &expires})
	require.NoError(t, err)
	checkState("active")
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
	<-time.After(time.Until(expires) + 20*time.Millisecond)
	checkState("expired")
	importFile("expired.pdf", mistraltest.MinimalPDF("expired consent"))
	_, err = service.RevokeConsent(t.Context())
	require.NoError(t, err)
	checkState("revoked")
	importFile("next.pdf", mistraltest.MinimalPDF("after revocation"))
	_, queued, err = catalog.ClaimNextRenditionJob(t.Context(), "test-worker", time.Now(), time.Minute)
	require.NoError(t, err)
	require.False(t, queued)
	cfg.Watches[0].Profile = "unavailable"
	listed, err := client.API().ListWatchedInboxes(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, listed.Items[0].Processing.Error)
	jobList, err := client.API().ListJobs(t.Context())
	require.NoError(t, err)
	require.Len(t, jobList.Items, 2)
	for _, job := range jobList.Items {
		switch job.Name {
		case "watch:inbox":
			require.Equal(t, listed.Items[0].Processing, job.Processing)
			require.Empty(t, job.Error, "consent failures stay separate from scanner errors")
		case "derive:visual-previews":
			require.True(t, job.Controllable)
			_, err := client.API().SetLaneControl(t.Context(), &apiclient.SetLaneControlRequestOptions{PathParams: &apiclient.SetLaneControlPath{Lane: job.Lane}, Body: &api.SetLaneControlRequest{Paused: true, Concurrency: 1}, Header: &apiclient.SetLaneControlHeaders{IfMatch: fmt.Sprintf("\"%d\"", job.ControlRevision)}})
			require.NoError(t, err)
		}
	}
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
