package store_test

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	productionservice "go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionPrivilegeDraftHTTPUsesSealedStoredMembership(t *testing.T) {
	vault, root, set, draft, member := store.ProductionReviewHTTPFixture(t)
	_, err := vault.SealProductionMembership(t.Context(), "synthetic-operator", set.ID, 1,
		api.ProductionMembershipSealRequest{
			OperationID: "89898989-8989-4898-8898-898989898901",
			Total:       1, MemberHash: draft.MemberHash,
		}.Domain(draft.ETag))
	require.NoError(t, err)
	withheld, err := productionservice.PrepareWithheldSelection(
		"89898989-8989-4898-8898-898989898902", documentproduction.WithheldSelection{
			Contract: documentproduction.WithheldSelectionContractV1,
			ID:       "89898989-8989-4898-8898-898989898903", SetID: set.ID, Revision: 1,
			PolicySHA256: draft.Policy.PolicySHA256,
			Members: []documentproduction.WithheldMember{{
				ID: member.ID, Ordinal: member.Ordinal, SourceVersionID: member.SourceVersionID,
				SourceSHA256: member.SourceSHA256, SourceSize: member.SourceSize,
				FamilyOrder: 1, Family: member.Family,
			}},
		}, nil)
	require.NoError(t, err)
	_, err = vault.PutProductionWithheldSelection(t.Context(), withheld)
	require.NoError(t, err)
	players, err := productionservice.PreparePlayersSnapshot(
		"89898989-8989-4898-8898-898989898904", documentproduction.PlayersSnapshot{
			Contract: documentproduction.PlayersSnapshotContractV1,
			ID:       "89898989-8989-4898-8898-898989898905", Revision: 1,
			Players: []documentproduction.Player{{
				ID: "89898989-8989-4898-8898-898989898906", DisplayName: "Synthetic Person",
				EvidenceSHA256: member.SourceSHA256,
			}},
		})
	require.NoError(t, err)
	_, err = vault.PutProductionPlayersSnapshot(t.Context(), players)
	require.NoError(t, err)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	const logID = "89898989-8989-4898-8898-898989898907"
	path := "/api/v1/production-privilege-logs/" + logID + "/revisions/1/draft"
	request := api.ProductionPrivilegeDraftCreateRequest{
		OperationID: "89898989-8989-4898-8898-898989898908",
		SetID:       set.ID, SetRevision: 1, PlayersSHA256: players.SnapshotSHA256,
		Rows: []documentproduction.PrivilegeRow{{
			ID: "89898989-8989-4898-8898-898989898909", WithheldMemberID: member.ID,
			FamilyOrder: 1, SourceVersionID: member.SourceVersionID, Basis: "synthetic_basis",
			PublicDescription: "Synthetic public description.",
			PrivateRationale:  "Synthetic private rationale.", EvidenceSHA256: member.SourceSHA256,
			PersonIDs: []string{players.Snapshot.Players[0].ID},
			Fields:    []documentproduction.PrivilegeField{{Name: "date", Value: "2026-09-22"}},
		}},
	}
	post := func(body api.ProductionPrivilegeDraftCreateRequest, key string) (int, []byte) {
		t.Helper()
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		httpRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			httpServer.URL+path, bytes.NewReader(encoded))
		require.NoError(t, err)
		httpRequest.Header.Set("Content-Type", "application/json")
		httpRequest.Header.Set("X-Api-Key", key)
		response, err := httpServer.Client().Do(httpRequest)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, data
	}
	status, body := post(request, "")
	require.Equal(t, http.StatusUnauthorized, status, string(body))
	status, body = post(request, cfg.Server.APIKey)
	require.Equal(t, http.StatusCreated, status, string(body))
	var created api.ProductionPrivilegeDraftGeneration
	require.NoError(t, json.Unmarshal(body, &created))
	require.Equal(t, logID, created.LogID)
	require.Equal(t, int64(1), created.Generation)
	require.NotContains(t, string(body), "Synthetic private rationale.")
	status, replay := post(request, cfg.Server.APIKey)
	require.Equal(t, http.StatusCreated, status, string(replay))
	require.Equal(t, body, replay)
	clientCreated, err := daemonconn.New(httpServer.URL, cfg.Server.APIKey).
		CreateProductionPrivilegeLogDraft(t.Context(), logID, 1, request)
	require.NoError(t, err)
	require.Equal(t, created, clientCreated)
	request.Rows[0].PrivateRationale = "Changed synthetic rationale."
	status, body = post(request, cfg.Server.APIKey)
	require.Equal(t, http.StatusConflict, status, string(body))
	require.NotContains(t, string(body), request.Rows[0].PrivateRationale)
}
