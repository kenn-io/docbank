package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestProductionWithheldCLIRecordsExactSelection(t *testing.T) {
	setID := "88000000-0000-4000-8000-000000000001"
	versionID := "88000000-0000-4000-8000-000000000004"
	request := api.ProductionWithheldSelectionCreateRequest{
		OperationID:  "88000000-0000-4000-8000-000000000002",
		SelectionID:  "88000000-0000-4000-8000-000000000003",
		PolicySHA256: strings.Repeat("a", 64),
		Members: []documentproduction.WithheldMember{{
			ID: "88000000-0000-4000-8000-000000000005", Ordinal: 1,
			SourceVersionID: versionID, SourceSHA256: strings.Repeat("b", 64),
			SourceSize: 100, FamilyOrder: 1,
			Family: redaction.FamilyContext{Kind: "standalone", RootVersionID: versionID},
		}},
	}
	raw, err := json.Marshal(request)
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "synthetic-withheld.json")
	require.NoError(t, os.WriteFile(file, raw, 0o600))
	result := documentproduction.WithheldSelection{
		Contract: documentproduction.WithheldSelectionContractV1,
		ID:       request.SelectionID, SetID: setID, Revision: 2,
		PolicySHA256: request.PolicySHA256, Members: request.Members,
	}
	_, result.SHA256, err = documentproduction.CanonicalWithheldSelection(result)
	require.NoError(t, err)
	var creates int
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		creates++
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
		assert.Equal(t, "/api/v1/productions/sets/"+setID+"/revisions/2/withheld-selection", r.URL.Path)
		var actual api.ProductionWithheldSelectionCreateRequest
		assert.NoError(t, json.UnmarshalRead(r.Body, &actual))
		assert.Equal(t, request, actual)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		assert.NoError(t, json.MarshalWrite(w, result))
	}))
	t.Cleanup(daemon.Close)
	ensureCalls := 0
	ensure := func(context.Context) (*daemonconn.Connection, error) {
		ensureCalls++
		return daemonconn.New(daemon.URL, "synthetic-key"), nil
	}
	cmd := newProductionWithheldCreateCommandWithEnsure(ensure)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{setID, "2", "--file", file, "--json"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	var decoded documentproduction.WithheldSelection
	require.NoError(t, json.Unmarshal(output.Bytes(), &decoded))
	require.Equal(t, result, decoded)
	require.Equal(t, 1, creates)
	require.Equal(t, 1, ensureCalls)
	invalid := newProductionWithheldCreateCommandWithEnsure(ensure)
	invalid.SetArgs([]string{setID, "0", "--file", file})
	require.Error(t, invalid.ExecuteContext(t.Context()))
	require.Equal(t, 1, ensureCalls)
}
