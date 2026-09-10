package docbank

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	internalprocessing "go.kenn.io/docbank/internal/processing"
)

func TestAwaitSubmitProcessingResultReturnsDurableJobWhenCancellationWins(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	started := make(chan internalprocessing.Job, 1)
	finished := make(chan submitProcessingResult, 1)
	finished <- submitProcessingResult{
		job: internalprocessing.Job{ID: "durable-processing-job"},
		err: errors.New("worker completed after admission"),
	}

	job, err := awaitSubmitProcessingResult(ctx, started, finished)
	require.NoError(t, err)
	require.Equal(t, "durable-processing-job", job.ID)
}
