package store_test

import (
	"encoding/json/v2"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionRequiredApprovalSelectedBeforeHTTPFinalization(t *testing.T) {
	vault, root, setID, revision, etag, namespaceID, approvalID := store.ProductionRequiredApprovalHTTPFixture(t)
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-test-key"
	server := api.NewServer(api.Deps{Store: vault, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	client := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	command := api.ProductionFinalizeRequest{
		OperationID: "76000000-0000-4000-8000-000000000024",
		NamespaceID: namespaceID, SnapshotID: "76000000-0000-4000-8000-000000000025",
	}
	_, err := client.FinalizeProductionDraft(t.Context(), setID, revision, etag, command)
	require.ErrorContains(t, err, "invalid_production")
	request := api.ProductionGateSelectionRequest{ApprovalID: approvalID}
	require.NoError(t, client.SelectProductionGateAuthority(t.Context(), setID, revision, request))
	require.NoError(t, client.SelectProductionGateAuthority(t.Context(), setID, revision, request))
	public, err := client.ProductionApproval(t.Context(), approvalID)
	require.NoError(t, err)
	require.Equal(t, approvalID, public.Grant.ID)
	encoded, err := json.Marshal(public)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "Synthetic approval evidence.")
	finalized, err := client.FinalizeProductionDraft(t.Context(), setID, revision, etag, command)
	require.NoError(t, err)
	require.Equal(t, "finalized", finalized.Draft.State)
}

func TestProductionGateSelectionReplaysThroughHTTPAndEmbeddedVault(t *testing.T) {
	vault, root, setID, revision, _, _ := store.ProductionFinalizeHTTPFixture(t)
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-test-key"
	server := api.NewServer(api.Deps{Store: vault, VaultRoot: root, Cfg: cfg})
	httpServer := httptest.NewServer(server.Handler())
	client := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	request := api.ProductionGateSelectionRequest{}
	require.NoError(t, client.SelectProductionGateAuthority(t.Context(), setID, revision, request))
	require.NoError(t, client.SelectProductionGateAuthority(t.Context(), setID, revision, request))
	require.Error(t, client.SelectProductionGateAuthority(t.Context(), setID, revision,
		api.ProductionGateSelectionRequest{ApprovalID: "7a000000-0000-4000-8000-000000000002"}))
	httpServer.Close()
	server.Close()
	require.NoError(t, vault.Close())

	embedded, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, embedded.Close()) })
	require.NoError(t, embedded.SelectProductionGateAuthority(t.Context(), setID, revision, request))
	require.Error(t, embedded.SelectProductionGateAuthority(t.Context(), setID, revision,
		api.ProductionGateSelectionRequest{ApprovalID: "7a000000-0000-4000-8000-000000000002"}))
}
