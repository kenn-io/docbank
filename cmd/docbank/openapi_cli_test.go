package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAPICommand(t *testing.T) {
	out, err := runCLI(t, "openapi")
	require.NoError(t, err)
	assert.Contains(t, out, "openapi: 3", "output is the YAML document")
	assert.Contains(t, out, "/api/v1/nodes")
}
