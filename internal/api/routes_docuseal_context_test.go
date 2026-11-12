package api

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDocuSealContextProblem(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{context.Canceled, 408, "docuseal_canceled"},
		{context.DeadlineExceeded, 504, "docuseal_timeout"},
	} {
		problem := docuSealProblem(fmt.Errorf("private provider detail: %w", tc.err))
		require.Equal(t, tc.status, problem.Status)
		require.Equal(t, tc.code, problem.Code)
		require.NotContains(t, problem.Error(), "private provider detail")
	}
}
