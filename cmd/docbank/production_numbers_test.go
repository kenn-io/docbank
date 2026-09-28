package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestProductionNumberCLIUsesDaemonForExactRangeAndCandidates(t *testing.T) {
	const label = "SYN000001"
	const namespaceID = "81818181-8181-4181-8181-818181818181"
	reference := api.ProductionNumberReference{
		Label: label, JobID: "82828282-8282-4282-8282-828282828282",
		SourceVersionID: "83838383-8383-4383-8383-838383838383",
		ArtifactID:      "synthetic-artifact", Volume: "VOL001",
	}
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(t, "synthetic-api-key", r.Header.Get("X-Api-Key"))
		assert.Equal(t, http.MethodGet, r.Method)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/productions/numbers":
			if r.URL.Query().Get("label") == label {
				assert.Empty(t, r.URL.Query().Get("namespace_id"))
				assert.NoError(t, json.MarshalWrite(w, api.ProductionNumberPage{
					Items: []api.ProductionNumberReference{reference}}))
				return
			}
			assert.Equal(t, namespaceID, r.URL.Query().Get("namespace_id"))
			assert.Equal(t, "1", r.URL.Query().Get("start_sequence"))
			assert.Equal(t, "2", r.URL.Query().Get("end_sequence"))
			assert.Equal(t, "1", r.URL.Query().Get("limit"))
			assert.NoError(t, json.MarshalWrite(w, api.ProductionNumberPage{
				Items: []api.ProductionNumberReference{reference}, NextSequence: 1}))
		case "/api/v1/productions/numbers/candidates":
			assert.Equal(t, "SYN", r.URL.Query().Get("query"))
			assert.Equal(t, "1", r.URL.Query().Get("limit"))
			assert.NoError(t, json.MarshalWrite(w, api.ProductionNumberCandidates{
				MatchKind: "prefix", Items: []api.ProductionNumberReference{reference},
				Ambiguous: true, Truncated: true}))
		default:
			assert.Fail(t, "unexpected production number route", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	ensure := func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(server.URL, "synthetic-api-key"), nil
	}
	find := newProductionNumberFindCommandWithEnsure(ensure)
	var output bytes.Buffer
	find.SetOut(&output)
	find.SetArgs([]string{label, "--json"})
	require.NoError(t, find.ExecuteContext(t.Context()))
	var exact api.ProductionNumberPage
	require.NoError(t, json.Unmarshal(output.Bytes(), &exact))
	require.Equal(t, reference, exact.Items[0])

	rangeCommand := newProductionNumberRangeCommandWithEnsure(ensure)
	output.Reset()
	rangeCommand.SetOut(&output)
	rangeCommand.SetArgs([]string{namespaceID, "1", "2", "--limit", "1", "--json"})
	require.NoError(t, rangeCommand.ExecuteContext(t.Context()))
	var ranged api.ProductionNumberPage
	require.NoError(t, json.Unmarshal(output.Bytes(), &ranged))
	require.Equal(t, reference, ranged.Items[0])
	require.EqualValues(t, 1, ranged.NextSequence)

	candidatesCommand := newProductionNumberCandidatesCommandWithEnsure(ensure)
	output.Reset()
	candidatesCommand.SetOut(&output)
	candidatesCommand.SetArgs([]string{"SYN", "--limit", "1", "--json"})
	require.NoError(t, candidatesCommand.ExecuteContext(t.Context()))
	var candidates api.ProductionNumberCandidates
	require.NoError(t, json.Unmarshal(output.Bytes(), &candidates))
	require.Equal(t, "prefix", candidates.MatchKind)
	require.True(t, candidates.Ambiguous)
	require.True(t, candidates.Truncated)
	require.Equal(t, reference, candidates.Items[0])
	textFind := newProductionNumberFindCommandWithEnsure(ensure)
	output.Reset()
	textFind.SetOut(&output)
	textFind.SetArgs([]string{label})
	require.NoError(t, textFind.ExecuteContext(t.Context()))
	require.Contains(t, output.String(), reference.ArtifactID)
	require.Contains(t, output.String(), reference.SourceVersionID)
	textRange := newProductionNumberRangeCommandWithEnsure(ensure)
	output.Reset()
	textRange.SetOut(&output)
	textRange.SetArgs([]string{namespaceID, "1", "2", "--limit", "1"})
	require.NoError(t, textRange.ExecuteContext(t.Context()))
	require.Contains(t, output.String(), "next_sequence: 1")
	textCandidates := newProductionNumberCandidatesCommandWithEnsure(ensure)
	output.Reset()
	textCandidates.SetOut(&output)
	textCandidates.SetArgs([]string{"SYN", "--limit", "1"})
	require.NoError(t, textCandidates.ExecuteContext(t.Context()))
	require.True(t, strings.HasPrefix(output.String(), "match=prefix ambiguous=true truncated=true\n"))
	require.Equal(t, 6, requests)
}

func TestProductionNumberCLICommandsRegistered(t *testing.T) {
	for _, leaf := range []string{"find", "range", "candidates"} {
		command, _, err := rootCmd.Find([]string{"production", "numbers", leaf})
		require.NoError(t, err)
		require.Equal(t, leaf, command.Name())
	}
}

func TestProductionNumberCLIRejectsInvalidSelectorsBeforeDaemon(t *testing.T) {
	var ensures int
	ensure := func(context.Context) (*daemonconn.Connection, error) {
		ensures++
		return nil, errors.New("daemon acquisition must not run for invalid selectors")
	}
	for _, test := range []struct {
		command *cobra.Command
		args    []string
	}{
		{newProductionNumberFindCommandWithEnsure(ensure), []string{" bad label "}},
		{newProductionNumberRangeCommandWithEnsure(ensure), []string{
			"81818181-8181-4181-8181-818181818181", "0", "2"}},
		{newProductionNumberCandidatesCommandWithEnsure(ensure), []string{"SYN", "--limit", "26"}},
	} {
		test.command.SetArgs(test.args)
		require.Error(t, test.command.ExecuteContext(t.Context()))
	}
	require.Zero(t, ensures)
}
