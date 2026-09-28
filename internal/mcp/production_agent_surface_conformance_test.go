package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The core production workflow and the later policy, log and retained-output
// workflow must be available from the same MCP server construction.
func TestProductionAgentSurfaceCombinesCoreAndExtendedTools(t *testing.T) {
	readOnly := catalogMap(toolCatalog(false))
	for _, name := range []string{
		"list_production_sets", "get_production_set", "get_production_draft",
		"list_production_members", "list_production_decisions", "get_production_job",
		"list_production_policies", "get_production_policy", "get_production_approval",
		"get_production_privilege_log", "get_production_package",
		"get_production_supplement", "get_production_reproduction",
	} {
		tool := readOnly[name]
		require.NotNil(t, tool, name)
		require.True(t, tool.Annotations.ReadOnlyHint, name)
	}
	for _, name := range []string{
		"create_production_set", "fork_production_draft", "append_production_members",
		"apply_production_changes", "seal_production_membership", "review_production_member",
		"select_production_gate_authority",
		"finalize_production_draft", "admit_production_job", "publish_production_package",
		"create_production_policy", "create_production_players_snapshot",
		"create_production_withheld_selection", "create_production_privilege_draft",
		"validate_production_privilege_log", "freeze_production_privilege_log",
		"create_production_package", "create_production_supplement",
		"create_production_reproduction",
	} {
		require.Nil(t, readOnly[name], name)
		tool := catalogMap(toolCatalog(true))[name]
		require.NotNil(t, tool, name)
		require.False(t, tool.Annotations.ReadOnlyHint, name)
	}
}
