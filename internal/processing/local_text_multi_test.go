package processing

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/jobs"
)

func TestLocalTextRestoredProfilesBackfillAndSkipIndependently(t *testing.T) {
	provider := &localTextEmbeddingProvider{descriptor: embeddingWorkerDescriptor(t)}
	fixture, original := newLocalTextBackfillFixture(t, newWorkerProvider(t), provider)
	source := original.Service.profiles["pdf"]
	profile := func(name string) ProfileConfig {
		portable := source.portable
		portable.Rendition = new(*portable.Rendition)
		portable.Rendition.AuthorizationFingerprint = workerHash(name)
		return ProfileConfig{Profile: portable, RenditionProvider: source.provider,
			EmbeddingProviders: source.embedders, Tokenizers: source.tokenizers}
	}
	service, err := NewService(t.Context(), ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), Profiles: map[string]ProfileConfig{"alpha": profile("alpha"), "beta": profile("beta")}, SpoolDirectory: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(service.Stop)
	for _, name := range []string{"alpha", "beta"} {
		require.NoError(t, service.grantProfileConsent(t.Context(), service.profiles[name], nil))
	}
	setup := &LocalTextSetup{Service: service, SelectedProfiles: []string{"beta", "alpha"}}
	lanes := setup.selectedLanes()
	items, err := lanes[0].targets(t.Context(), "", 100)
	require.NoError(t, err)
	require.NotEmpty(t, items)
	for _, item := range items {
		lanes[0].skip(item, "")
	}
	require.Empty(t, lanes[1].skips)
	firstPending, err := lanes[0].Pending(t.Context())
	require.NoError(t, err)
	require.False(t, firstPending)
	pending, err := setup.Pending(t.Context())
	require.NoError(t, err)
	require.True(t, pending)
	lanes[0].skips = nil
	supervisor := jobs.New(t.Context(), slog.New(slog.DiscardHandler))
	defer func() { require.NoError(t, supervisor.Shutdown(context.WithoutCancel(t.Context()))) }()
	require.NoError(t, supervisor.StartWorker("text:local", func(ctx context.Context) error { return setup.Backfill(ctx, nil) }))
	require.NoError(t, supervisor.Settle(t.Context()))
	written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.PDF()))
	require.NoError(t, err)
	node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "new.pdf", written.Hash, written.Size, "application/pdf", processingBlobPhysical(t, written))
	require.NoError(t, err)
	require.NoError(t, supervisor.Settle(t.Context()))
	for _, name := range []string{"alpha", "beta"} {
		jobs, err := fixture.catalog.EmbeddingJobsForVersionProfile(t.Context(), node.CurrentVersionID, service.profiles[name].record.Fingerprint)
		require.NoError(t, err)
		require.Len(t, jobs, 1, name)
		require.Equal(t, "completed", jobs[0].State)
	}
}
