package processing

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/store"
)

func TestLocalTextMoveWithPendingProviderFailure(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var failing atomic.Bool
		var attempts atomic.Int64
		provider := &localTextEmbeddingProvider{descriptor: embeddingWorkerDescriptor(t), embed: func(context.Context) error {
			if failing.Load() {
				attempts.Add(1)
				return errors.New("synthetic pending-document provider outage")
			}
			return nil
		}}
		fixture, original := newLocalTextBackfillFixture(t, newWorkerProvider(t), provider)
		source := original.Service
		written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.PDF()))
		require.NoError(t, err)
		indexed, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "indexed.pdf", written.Hash, written.Size, "application/pdf", processingBlobPhysical(t, written))
		require.NoError(t, err)
		selector := Selector{Profile: "pdf", NodeID: indexed.ID, ContentVersionID: indexed.CurrentVersionID}
		plan, err := source.Plan(t.Context(), selector)
		require.NoError(t, err)
		_, err = source.Start(t.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
		require.NoError(t, err)
		request := SearchRequest{Profile: "pdf", Query: "synthetic", Mode: "semantic", Limit: 10, AllLive: &store.SearchOptions{}}
		before, err := source.Search(t.Context(), request)
		require.NoError(t, err)
		require.Len(t, before.Results, 1)
		saved := "previous-profile"
		setup := newLocalTextMoveSetup(t, source, &saved)
		move, err := setup.Plan(t.Context(), "local")
		require.NoError(t, err)
		require.Contains(t, move.VectorEffect, "processes 2 documents before setup returns")
		_, err = fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "pending.pdf", written.Hash, written.Size, "application/pdf", processingBlobPhysical(t, written))
		require.NoError(t, err)
		failing.Store(true)
		_, err = setup.Apply(t.Context(), "local", move.Fingerprint)
		require.ErrorIs(t, err, ErrConsentSaved)
		require.ErrorContains(t, err, string(store.EmbeddingFailureProviderUnavailable))
		require.Equal(t, "previous-profile", saved)
		require.Positive(t, attempts.Load())
		failing.Store(false)
		after, err := source.Search(t.Context(), request)
		require.NoError(t, err)
		require.Len(t, after.Results, 1)
		require.Equal(t, before.Results[0].Document, after.Results[0].Document)
	})
}
