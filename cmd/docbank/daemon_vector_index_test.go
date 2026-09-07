package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

func TestStartVectorIndexWorkerUsesSupervisorLifecycle(t *testing.T) {
	starter := &fakeEmbeddingJobStarter{}
	err := startVectorIndexWorker(starter, func() (embeddingJobRunner, error) {
		return embeddingRunnerFunc(func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}), nil
	})
	require.NoError(t, err)
	assert.Equal(t, "process:vector-indexes", starter.name)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, starter.run(ctx), context.Canceled)

	want := errors.New("synthetic vector index configuration failure")
	err = startVectorIndexWorker(&fakeEmbeddingJobStarter{}, func() (embeddingJobRunner, error) {
		return nil, want
	})
	require.ErrorIs(t, err, want)
}

func TestVectorIndexRetryPolicyClassifiesStoreFailures(t *testing.T) {
	retryable := vectorIndexRetryPolicy(store.DefaultSQLiteDriver().IsBusy)
	for _, testCase := range []struct {
		name  string
		cause error
		want  bool
	}{
		{"temporarily unavailable", fmt.Errorf("reading vector payload: %w", packstore.ErrStoreUnavailable), true},
		{"physically missing", packstore.ErrPhysicalMissing, false},
		{"corrupt", packstore.ErrPhysicalCorrupt, false},
		{"fenced", packstore.ErrStoreFenced, false},
		{"unavailable and corrupt", errors.Join(packstore.ErrStoreUnavailable, packstore.ErrPhysicalCorrupt), false},
		{"unavailable and fenced", errors.Join(packstore.ErrStoreUnavailable, packstore.ErrStoreFenced), false},
	} {
		t.Run(testCase.name, func(t *testing.T) { require.Equal(t, testCase.want, retryable(testCase.cause)) })
	}
}
