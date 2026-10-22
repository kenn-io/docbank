package api

import (
	"context"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/redactiontest"
)

func TestRedactionProblemCauses(t *testing.T) {
	t.Parallel()
	memory := &pdfproduction.RSSLimitError{ObservedBytes: 2048, LimitBytes: 1024}
	p := redactionProblem(t.Context(), memory)
	require.Equal(t, 503, p.Status)
	require.Equal(t, "redaction_memory", p.Code)
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	p = redactionProblem(ctx, memory)
	require.Equal(t, 504, p.Status)
	require.Equal(t, "redaction_timeout", p.Code)
	p = redactionProblem(t.Context(), context.DeadlineExceeded)
	require.Equal(t, 503, p.Status)
	require.Equal(t, "redaction_worker_timeout", p.Code)
	p = redactionProblem(t.Context(), context.Canceled)
	require.Equal(t, 500, p.Status)
	require.Equal(t, "redaction_failed", p.Code)
	p = redactionProblem(t.Context(), &processing.NativePDFBusyError{Operation: "render"})
	require.Equal(t, 503, p.Status)
	require.Contains(t, p.Detail, "render")
}

func TestRedactionProblemFullEnvelopeBound(t *testing.T) {
	t.Parallel()
	problem := redactiontest.Expansion(t, pdfproduction.QualifiedRecipe())
	full := redactionProblem(t.Context(), problem)
	require.Equal(t, problem, full.Redaction)
	encoded, err := json.Marshal(problem)
	require.NoError(t, err)
	// The extension itself fits; the entire transport envelope does not.
	p := boundedRedactionProblem(t.Context(), problem, int64(len(encoded))+1)
	require.Equal(t, "redaction_limit", p.Code)
	require.Nil(t, p.Redaction, "an expansion must never be truncated")
}
