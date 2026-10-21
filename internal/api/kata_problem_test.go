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
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
		detail string
	}{
		{"invalid_reference", &kataclient.Error{Code: "invalid_reference"}, 422, "invalid_kata_evidence", ""},
		{"wrong_project", &kataclient.Error{Code: "wrong_project"}, 422, "invalid_kata_evidence", ""},
		{"project_unavailable", &kataclient.Error{Code: "project_unavailable"}, 422, "invalid_kata_evidence", ""},
		{"unsupported", kataevidence.ErrUnsupported, 409, "kata_evidence_unsupported", ""},
		{"unauthorized", &kataclient.Error{Status: 401, Code: "http_error"}, 401, "kata_unauthorized", ""},
		{"forbidden", &kataclient.Error{Status: 403, Code: "http_error"}, 403, "kata_forbidden", ""},
		{"unavailable", &kataclient.Error{Status: 404, Code: "http_error"}, 404, "kata_issue_unavailable", ""},
		{"invalid_range", document.ErrInvalidCitationRange, 422, "invalid_kata_evidence", "--end"},
		{"entry_limit", kataevidence.ErrEntryLimit, 409, "kata_evidence_limit", "issue already holds the maximum of 64 Docbank quotes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problem := kataProblem(tc.err)
			require.Equal(t, tc.status, problem.Status)
			require.Equal(t, tc.code, problem.Code)
			if tc.detail != "" {
				require.Contains(t, problem.Detail, tc.detail)
			}
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
