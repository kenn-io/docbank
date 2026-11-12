package mcp

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContractCatalogOptIn(t *testing.T) {
	names := func(options ServerOptions) map[string]bool {
		result := map[string]bool{}
		for _, tool := range toolCatalog(options) {
			result[tool.Name] = true
		}
		return result
	}
	read := names(ServerOptions{})
	assert.True(t, read["list_contracts"])
	assert.True(t, read["get_contract"])
	assert.False(t, read["create_contract"])
	writes := names(ServerOptions{AllowContractWrites: true})
	for _, name := range []string{"create_contract", "update_contract", "link_contract_document"} {
		assert.True(t, writes[name])
	}
}

func TestContractPatchPreservesNull(t *testing.T) {
	patch, err := decodeContractPatch([]byte(`{"starts_on":null,"expires_on":"2026-12-01","summary":""}`))
	require.NoError(t, err)
	encoded, err := json.Marshal(patch)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(encoded, &fields))
	assert.Contains(t, fields, "starts_on")
	assert.Nil(t, fields["starts_on"])
	assert.Equal(t, "2026-12-01", fields["expires_on"])
	assert.Equal(t, "", fields["summary"])
	assert.NotContains(t, fields, "signed_on")
}

func TestContractInputSemanticsTermsByteLimit(t *testing.T) {
	assert.True(t, validToolSemantics("update_contract", map[string]any{"patch": map[string]any{"summary": strings.Repeat("é", 4096)}}))
	assert.False(t, validToolSemantics("update_contract", map[string]any{"patch": map[string]any{"summary": strings.Repeat("é", 4097)}}))
}
