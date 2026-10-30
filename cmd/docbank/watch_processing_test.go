package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/mistral/mistraltest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestWatchMistralImportConsentAndExactVersions(t *testing.T) {
	cfg, _ := mistralOCRProcessingConfig(t)
	profiles, err := executableProcessingProfiles(cfg, embeddingRuntimeBundle{})
	require.NoError(t, err)
	changed := profiles["documents"]
	changed.Profile.RetentionDisclosure.ConsentFingerprint = strings.Repeat("f", 64)
	profiles["changed"] = changed
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
	server := api.NewServer(api.Deps{Store: catalog, Blobs: blobs, VaultRoot: root, Cfg: cfg, Processing: service})
	defer server.Close()
	endpoint := httptest.NewServer(server.Handler())
	defer endpoint.Close()
	client := daemonconn.New(endpoint.URL, cfg.Server.APIKey)
	defer func() { require.NoError(t, client.Close()) }()
	checkState := func(want string) {
		t.Helper()
		listed, err := client.API().ListWatchedInboxes(t.Context())
		require.NoError(t, err)
		require.Equal(t, "documents", listed.Items[0].Profile)
		require.Equal(t, want, listed.Items[0].ConsentState)
	}
	checkState("required")
	watcher, err := ingest.NewWatcher(&ingest.Ingester{Store: catalog, Blobs: blobs}, root, watch, gate.Mutate, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	imports := make(chan store.Node, 8)
	watcher.AfterImport = func(ctx context.Context, node store.Node) error {
		err := service.AdmitImportedRendition(ctx, node, watch.Profile)
		imports <- node
		return err
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- watcher.Run(ctx) }()
	defer func() { cancel(); <-done }()
	importFile := func(name, content string) store.Node {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(source, name), mistraltest.MinimalPDF(content), 0600))
		select {
		case node := <-imports:
			return node
		case <-time.After(10 * time.Second):
			t.Fatal("watch import did not finish")
			return store.Node{}
		}
	}
	first := importFile("report.pdf", "without consent")
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
	cfg.Watches[0].Profile = "changed"
	changedStatus, err := client.API().ListWatchedInboxes(t.Context())
	require.NoError(t, err)
	require.Equal(t, "changed", changedStatus.Items[0].Profile)
	require.Equal(t, "required", changedStatus.Items[0].ConsentState)
	cfg.Watches[0].Profile = "documents"
	// Consent does not backfill the already accepted version.
	_, queued, err = catalog.ClaimNextRenditionJob(t.Context(), "test-worker", time.Now(), time.Minute)
	require.NoError(t, err)
	require.False(t, queued)
	updated := importFile("report.pdf", "with consent")
	require.Equal(t, first.ID, updated.ID)
	require.NotEqual(t, first.CurrentVersionID, updated.CurrentVersionID)
	job, queued, err := catalog.ClaimNextRenditionJob(t.Context(), "test-worker", time.Now(), time.Minute)
	require.NoError(t, err)
	require.True(t, queued)
	work, err := catalog.RenditionJobWorkByClaim(t.Context(), job, time.Now())
	require.NoError(t, err)
	require.Equal(t, updated.CurrentVersionID, work.Waiter.ContentVersionID)
	t.Logf("required import %s: no job; consented update %s: durable job %s", first.CurrentVersionID, updated.CurrentVersionID, job.JobID)
	// An unsupported import still succeeds, and no second job appears.
	require.NoError(t, os.WriteFile(filepath.Join(source, "notes.txt"), bytes.Repeat([]byte("text"), 8), 0600))
	select {
	case <-imports:
	case <-time.After(10 * time.Second):
		t.Fatal("text import did not finish")
	}
	_, queued, err = catalog.ClaimNextRenditionJob(t.Context(), "test-worker", time.Now(), time.Minute)
	require.NoError(t, err)
	require.False(t, queued)
	<-time.After(time.Until(expires) + 20*time.Millisecond)
	checkState("expired")
	_, err = service.RevokeConsent(t.Context())
	require.NoError(t, err)
	checkState("revoked")
	revoked := importFile("next.pdf", "after revocation")
	_, queued, err = catalog.ClaimNextRenditionJob(t.Context(), "test-worker", time.Now(), time.Minute)
	require.NoError(t, err)
	require.False(t, queued)
	t.Logf("revoked import %s: accepted with no job; API consent required -> active -> expired -> revoked", revoked.CurrentVersionID)
	var output bytes.Buffer
	listed, err := client.API().ListWatchedInboxes(t.Context())
	require.NoError(t, err)
	require.NoError(t, json.MarshalWrite(&output, listed))
	require.Contains(t, output.String(), `"consent_state":"revoked"`)
}
