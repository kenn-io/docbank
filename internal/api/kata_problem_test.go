package api

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/kataclient"
	"go.kenn.io/docbank/internal/kataevidence"
)

func TestKataProblemClassifiesPermanentReferenceFailures(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"invalid_reference", "wrong_project", "project_unavailable"} {
		t.Run(code, func(t *testing.T) {
			problem := kataProblem(&kataclient.Error{Code: code})
			require.Equal(t, 422, problem.Status)
			require.Equal(t, "invalid_kata_evidence", problem.Code)
			require.NotContains(t, strings.ToLower(problem.Detail), "retry")
		})
	}
}

func TestKataProblemKeepsTransientFailuresRetryable(t *testing.T) {
	t.Parallel()
	problem := kataProblem(&kataclient.Error{Status: 502, Code: "request_failed"})
	require.Equal(t, 502, problem.Status)
	require.Equal(t, "kata_request_failed", problem.Code)
	require.Contains(t, problem.Detail, "retry")
}

func TestKataProblemMapsUnsupportedEvidenceAsPermanent(t *testing.T) {
	t.Parallel()
	problem := kataProblem(kataevidence.ErrUnsupported)
	require.Equal(t, 409, problem.Status)
	require.Equal(t, "kata_evidence_unsupported", problem.Code)
	require.NotContains(t, strings.ToLower(problem.Detail), "retry")
}

func TestKataProblemClassifiesPermanentUpstreamFailures(t *testing.T) {
	t.Parallel()
	for _, status := range []int{401, 403, 404} {
		problem := kataProblem(&kataclient.Error{Status: status, Code: "http_error"})
		require.Equal(t, status, problem.Status)
		require.NotContains(t, strings.ToLower(problem.Detail), "retry")
	}
}

func TestKataProblemRejectsInvalidCitationRange(t *testing.T) {
	t.Parallel()
	problem := kataProblem(document.ErrInvalidCitationRange)
	require.Equal(t, 422, problem.Status)
	require.Contains(t, problem.Detail, "--end")
	require.NotContains(t, problem.Detail, "retry")
}
