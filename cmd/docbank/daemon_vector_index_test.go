package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
