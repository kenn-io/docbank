package notemarkdown

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Cancellation becomes visible at deterministic work checkpoints, without
// clocks or a scheduler race.
type cancellationChecks struct {
	context.Context
	remaining int
}

func (c *cancellationChecks) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestCancellationAndBoundedWork(t *testing.T) {
	c := &cancellationChecks{Context: t.Context(), remaining: 10}
	n, err := Parse(c, []byte(strings.Repeat("a", 100000)), Limits{})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, Note{}, n)
	for _, tc := range []struct{ src, resource string }{
		{strings.Repeat("[", 129), "ast_depth"},
		{strings.Repeat("> ", 129) + "text", "ast_depth"},
		{"text " + strings.Repeat("<x ", 24000), "work_steps"},
		{strings.Repeat("[x][r] ", 5000) + "\n\n[r]: " + strings.Repeat("a", 4096), "derived_bytes"},
	} {
		n, err := Parse(t.Context(), []byte(tc.src), Limits{})
		requireLimit(t, err, tc.resource)
		require.Equal(t, Note{}, n)
	}
}
