package notemarkdown

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBudgetInclusiveLimits(t *testing.T) {
	for _, resource := range []string{"input_bytes", "frontmatter_bytes", "line_breaks", "headings", "links", "aliases", "tags", "target_bytes", "title_bytes", "metadata_value_bytes", "yaml_depth", "yaml_nodes", "ast_depth", "ast_nodes", "work_steps", "derived_bytes", "artifact_bytes", "diagnostics"} {
		t.Run(resource, func(t *testing.T) {
			value := 0
			require.NoError(t, addLimit(resource, &value, 32, 32))
			require.Equal(t, 32, value)
			requireLimit(t, addLimit(resource, &value, 1, 32), resource)
			require.Equal(t, 32, value)
			value = math.MaxInt - 1
			requireLimit(t, addLimit(resource, &value, math.MaxInt, math.MaxInt), resource)
		})
	}
	b := budget{}
	require.NoError(t, b.step(t.Context(), 16777216))
	requireLimit(t, b.step(t.Context(), 1), "work_steps")
	require.NoError(t, b.reserveStrings(8<<20))
	requireLimit(t, b.reserveStrings(1), "derived_bytes")
}
