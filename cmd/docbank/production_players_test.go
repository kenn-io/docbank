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
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestProductionPlayersCLIRecordsVerifiedSnapshot(t *testing.T) {
	snapshotID := "86000000-0000-4000-8000-000000000001"
	operationID := "86000000-0000-4000-8000-000000000002"
	players := []documentproduction.Player{{
		ID: "86000000-0000-4000-8000-000000000003", DisplayName: "Synthetic Person",
		Aliases: []string{"Person, Synthetic"}, EvidenceSHA256: strings.Repeat("a", 64),
	}}
	raw, err := json.Marshal(players)
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "synthetic-players.json")
	require.NoError(t, os.WriteFile(file, raw, 0o600))
	result := documentproduction.PlayersSnapshot{Contract: documentproduction.PlayersSnapshotContractV1,
		ID: snapshotID, Revision: 2, Players: players}
	_, result.SHA256, err = documentproduction.CanonicalPlayersSnapshot(result)
	require.NoError(t, err)
	var creates int
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		creates++
		assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/production-player-snapshots/"+snapshotID+"/revisions/2", r.URL.Path)
		var actual api.ProductionPlayersSnapshotCreateRequest
		assert.NoError(t, json.UnmarshalRead(r.Body, &actual))
		assert.Equal(t, api.ProductionPlayersSnapshotCreateRequest{OperationID: operationID, Players: players}, actual)
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
	cmd := newProductionPlayersCreateCommandWithEnsure(ensure)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{snapshotID, "2", "--operation-id", operationID, "--file", file, "--json"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	var decoded documentproduction.PlayersSnapshot
	require.NoError(t, json.Unmarshal(output.Bytes(), &decoded))
	require.Equal(t, result, decoded)
	require.Equal(t, 1, creates)
	require.Equal(t, 1, ensureCalls)

	invalid := newProductionPlayersCreateCommandWithEnsure(ensure)
	invalid.SetArgs([]string{snapshotID, "2", "--operation-id", "bad", "--file", file})
	require.Error(t, invalid.ExecuteContext(t.Context()))
	require.Equal(t, 1, ensureCalls)
}
