package mcp

import (
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
)

func syntheticMCPPlayersFile(t *testing.T) (string, documentproduction.PlayersSnapshot) {
	t.Helper()
	result := documentproduction.PlayersSnapshot{
		Contract: documentproduction.PlayersSnapshotContractV1,
		ID:       "87000000-0000-4000-8000-000000000001", Revision: 2,
		Players: []documentproduction.Player{{
			ID: "87000000-0000-4000-8000-000000000002", DisplayName: "Synthetic Private Name",
			Aliases: []string{"Private, Synthetic"}, EvidenceSHA256: strings.Repeat("a", 64),
		}},
	}
	var err error
	_, result.SHA256, err = documentproduction.CanonicalPlayersSnapshot(result)
	require.NoError(t, err)
	raw, err := json.Marshal(result.Players)
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "private-players.json")
	require.NoError(t, os.WriteFile(file, raw, 0o600))
	return file, result
}

func TestProductionPlayersToolUsesPrivateFileAndReturnsDigestOnly(t *testing.T) {
	file, result := syntheticMCPPlayersFile(t)
	operationID := "87000000-0000-4000-8000-000000000003"
	var creates int
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		creates++
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
		assert.Equal(t, "/api/v1/production-player-snapshots/"+result.ID+"/revisions/2", r.URL.Path)
		var actual api.ProductionPlayersSnapshotCreateRequest
		assert.NoError(t, json.UnmarshalRead(r.Body, &actual))
		assert.Equal(t, api.ProductionPlayersSnapshotCreateRequest{
			OperationID: operationID, Players: result.Players,
		}, actual)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		assert.NoError(t, json.MarshalWrite(w, result))
	}))
	t.Cleanup(daemon.Close)
	require.Nil(t, catalogMap(toolCatalog(false))["create_production_players_snapshot"])
	tool := catalogMap(toolCatalog(true))["create_production_players_snapshot"]
	require.NotNil(t, tool)
	args := map[string]any{"snapshot_id": result.ID, "revision": 2,
		"operation_id": operationID, "players_file": file}
	assertSchemaAccepts(t, tool.InputSchema, args)
	server := newBatesToolTestServer(t, daemon.URL, true)
	created := callToolResult(t, server, "create_production_players_snapshot", args)
	require.NotEqual(t, true, created["isError"], created)
	summary := objectField(t, created, "structuredContent")
	require.Equal(t, result.ID, summary["snapshot_id"])
	require.Equal(t, result.SHA256, summary["sha256"])
	require.EqualValues(t, 1, summary["player_count"])
	encoded, err := json.Marshal(created)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), result.Players[0].DisplayName)
	require.Equal(t, 1, creates)
	args["players_file"] = "relative-private-players.json"
	bad := decodeWireError(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "create_production_players_snapshot", "arguments": args,
	})))
	require.NotContains(t, bad.Message, result.Players[0].DisplayName)
	require.Equal(t, 1, creates)
}

func TestProductionPlayersToolDoesNotRetryUnknownOutcome(t *testing.T) {
	file, result := syntheticMCPPlayersFile(t)
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("{"))
	}))
	t.Cleanup(daemon.Close)
	created := callToolResult(t, newBatesToolTestServer(t, daemon.URL, true),
		"create_production_players_snapshot", map[string]any{
			"snapshot_id": result.ID, "revision": 2,
			"operation_id": "87000000-0000-4000-8000-000000000003", "players_file": file,
		})
	require.Equal(t, true, created["isError"])
	require.Equal(t, "production_players_outcome_unknown", objectField(t, created, "structuredContent")["code"])
	require.Equal(t, 1, calls)
}
