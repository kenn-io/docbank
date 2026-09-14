package main

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
)

func TestFormatsCommandJSONAndTable(t *testing.T) {
	_ = setupVaultHome(t)

	out, err := runCLI(t, "formats", "--extension", "wpd", "--json")
	require.NoError(t, err)
	var response api.FormatCoverageResponse
	require.NoError(t, json.Unmarshal([]byte(out), &response))
	require.NoError(t, document.ValidateFormatCoverageV1(response.FormatCoverageV1))
	require.NotNil(t, response.Lookup)
	assert.Equal(t, document.FormatLookupPending, response.Lookup.Match)
	assert.Equal(t, "wpd", response.Lookup.Query)
	assert.Equal(t, "DB-42b", response.Lookup.Pending.OwnerSlice)

	out, err = runCLI(t, "formats", "--format", "zip")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 8, "one header plus all seven capability rows")
	assert.Equal(t, "FORMAT  CAPABILITY  STATE           REASON", lines[0])
	assert.Contains(t, out, "zip     expand      unsupported")
	assert.Contains(t, out, "zip     retain      qualified")

	out, err = runCLI(t, "formats", "--extension", "wpd")
	require.NoError(t, err)
	assert.Contains(t, out, "QUERY")
	assert.Contains(t, out, "MATCH")
	assert.Contains(t, out, "OWNER")
	assert.Contains(t, out, "wpd")
	assert.Contains(t, out, "pending")
	assert.Contains(t, out, "DB-42b")
	assert.Contains(t, out, "WPD: WordPerfect document support is owned by DB-42b.")

	out, err = runCLI(t, "formats", "--extension", "qqq")
	require.NoError(t, err)
	assert.Contains(t, out, "qqq")
	assert.Contains(t, out, "unknown_format")
	assert.Contains(t, out, "No catalog or pending format matched.")

	out, err = runCLI(t, "formats", "--family", "archive", "--format", "pdf")
	require.NoError(t, err)
	assert.Contains(t, out, "pdf")
	assert.Contains(t, out, "format")
	assert.Contains(t, out, "Matched pdf in family document; excluded by --family archive.")

	_, err = runCLI(t, "formats", "--format", "zip", "--extension", "zip")
	require.Error(t, err)
	require.ErrorContains(t, err, "exactly one of --format or --extension")
	var exit *exitError
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, exitUsage, exit.code)
}
