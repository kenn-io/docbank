package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
)

func TestRedactionPreparationMemoryProblem(t *testing.T) {
	t.Parallel()
	refusal := fmt.Errorf("native inspection: %w", &pdfproduction.RSSLimitError{ObservedBytes: 2048, LimitBytes: 1024})
	problem := redactionPreparationProblem(t.Context(), refusal)
	require.Equal(t, 503, problem.Status)
	require.Equal(t, "redaction_preparation_memory", problem.Code)
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	problem = redactionPreparationProblem(expired, refusal)
	require.Equal(t, 504, problem.Status)
	require.Equal(t, "redaction_preparation_timeout", problem.Code)
	problem = redactionPreparationProblem(t.Context(), context.DeadlineExceeded)
	require.Equal(t, 503, problem.Status)
	require.Equal(t, "redaction_preparation_worker_timeout", problem.Code)
	problem = redactionPreparationProblem(t.Context(), context.Canceled)
	require.Equal(t, 500, problem.Status)
	problem = redactionPreparationProblem(t.Context(), processing.ErrRedactionPreparationUnavailable)
	require.Equal(t, 503, problem.Status)
	require.Equal(t, "redaction_preparation_unavailable", problem.Code)
}
