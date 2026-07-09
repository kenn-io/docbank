package cmd

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAPICommand(t *testing.T) {
	out, err := runCLI(t, "openapi")
	require.NoError(t, err)
	assert.Contains(t, out, "openapi: 3", "default output is the YAML document")
	assert.Contains(t, out, "/api/v1/nodes")

	out, err = runCLI(t, "openapi", "--json")
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &doc))
	assert.Contains(t, doc, "paths")
}
