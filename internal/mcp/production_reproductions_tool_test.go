package mcp

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/production"
)

func syntheticReproductionToolReceipt(t *testing.T) (string, api.ProductionReproductionCreateRequest,
	documentproduction.ReproductionReceipt) {
	t.Helper()
	jobID := "83000000-0000-4000-8000-000000000001"
	policy := production.PackageDeliveryPolicy{RecipientCode: "synthetic-recipient",
		AllowedMethods: []string{"offline-media"}}
	policySHA256, err := production.PackageDeliveryPolicySHA256(policy)
	require.NoError(t, err)
	request := documentproduction.ReproductionRequest{
		Contract:                        documentproduction.ReproductionRequestContractV1,
		OperationID:                     "83000000-0000-4000-8000-000000000002",
		OriginalProductionReceiptSHA256: strings.Repeat("a", 64),
		ArtifactIDs:                     []string{"83000000-0000-4000-8000-000000000003"},
		SourceVersionIDs:                []string{"83000000-0000-4000-8000-000000000004"},
		DeliveryPolicySHA256:            policySHA256,
	}
	input := api.ProductionReproductionCreateRequest{
		Request: request, DeliveryPolicy: api.ProductionReproductionDeliveryPolicy(policy),
		ProfileID: "export-dat-opt-images-v1", MaxVolumeBytes: 50 << 20,
		MaxVolumeDocuments: 10,
	}
	receipt := documentproduction.ReproductionReceipt{
		Contract: documentproduction.ReproductionReceiptContractV1, ID: request.OperationID,
		OriginalProductionReceiptSHA256: request.OriginalProductionReceiptSHA256,
		OriginalNumberReservationSHA256: strings.Repeat("b", 64),
		ArtifactManifestSHA256:          strings.Repeat("c", 64),
		PackageQCSHA256:                 strings.Repeat("d", 64), DeliveryPolicySHA256: policySHA256,
		CreatedAt: "2026-09-27T00:00:00Z",
	}
	_, receipt.SHA256, err = documentproduction.CanonicalReproductionReceipt(receipt)
	require.NoError(t, err)
	return jobID, input, receipt
}

func TestProductionReproductionToolsCreateAndReadExactReceipt(t *testing.T) {
	jobID, input, receipt := syntheticReproductionToolReceipt(t)
	require.Nil(t, catalogMap(toolCatalog(false))["create_production_reproduction"])
	require.NotNil(t, catalogMap(toolCatalog(false))["get_production_reproduction"])
	require.NotNil(t, catalogMap(toolCatalog(true))["create_production_reproduction"])
	var creates, reads int
	path := "/api/v1/productions/jobs/" + jobID + "/reproductions"
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case path:
			creates++
			assert.Equal(t, http.MethodPost, r.Method)
			var actual api.ProductionReproductionCreateRequest
			assert.NoError(t, json.UnmarshalRead(r.Body, &actual))
			assert.Equal(t, input, actual)
			w.WriteHeader(http.StatusCreated)
		case path + "/" + input.Request.OperationID:
			reads++
			assert.Equal(t, http.MethodGet, r.Method)
		default:
			http.NotFound(w, r)
			return
		}
		assert.NoError(t, json.MarshalWrite(w, receipt))
	}))
	t.Cleanup(daemon.Close)
	args := map[string]any{
		"job_id": jobID, "request": input.Request,
		"delivery_policy": input.DeliveryPolicy, "profile_id": input.ProfileID,
		"max_volume_bytes":     input.MaxVolumeBytes,
		"max_volume_documents": input.MaxVolumeDocuments,
	}
	encodedArgs, err := json.Marshal(args)
	require.NoError(t, err)
	var schemaArgs map[string]any
	require.NoError(t, json.Unmarshal(encodedArgs, &schemaArgs))
	assertSchemaAccepts(t, catalogMap(toolCatalog(true))["create_production_reproduction"].InputSchema, schemaArgs)
	first := callToolResult(t, newBatesToolTestServer(t, daemon.URL, true),
		"create_production_reproduction", args)
	require.NotEqual(t, true, first["isError"], first)
	summary := objectField(t, objectField(t, first, "structuredContent"), "receipt")
	require.Equal(t, receipt.SHA256, summary["sha256"])
	require.EqualValues(t, 0, summary["number_allocation_count"])
	args = schemaArgs
	delete(objectField(t, args, "request"), "delivery_policy_sha256")
	replayed := callToolResult(t, newBatesToolTestServer(t, daemon.URL, true),
		"create_production_reproduction", args)
	require.NotEqual(t, true, replayed["isError"], replayed)
	require.Equal(t, receipt.SHA256, objectField(t, objectField(t, replayed, "structuredContent"), "receipt")["sha256"])
	shown := callToolResult(t, newBatesToolTestServer(t, daemon.URL, false),
		"get_production_reproduction", map[string]any{
			"job_id": jobID, "operation_id": input.Request.OperationID,
		})
	require.NotEqual(t, true, shown["isError"], shown)
	require.Equal(t, receipt.SHA256, objectField(t, objectField(t, shown, "structuredContent"), "receipt")["sha256"])
	require.Equal(t, 2, creates)
	require.Equal(t, 1, reads)
}

func TestProductionReproductionToolPreservesConflictAndUnknownOutcome(t *testing.T) {
	jobID, input, _ := syntheticReproductionToolReceipt(t)
	args := map[string]any{
		"job_id": jobID, "request": input.Request,
		"delivery_policy": input.DeliveryPolicy, "profile_id": input.ProfileID,
		"max_volume_bytes":     input.MaxVolumeBytes,
		"max_volume_documents": input.MaxVolumeDocuments,
	}
	for _, scenario := range []struct {
		name, code string
		status     int
		body       string
	}{
		{"conflict", "production_reproduction_conflict", http.StatusConflict,
			`{"status":409,"code":"production_reproduction_conflict","detail":"Synthetic private context."}`},
		{"uncertain response", "production_reproduction_outcome_unknown", http.StatusCreated, "{"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var calls int
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(scenario.status)
				_, _ = w.Write([]byte(scenario.body))
			}))
			t.Cleanup(daemon.Close)
			result := callToolResult(t, newBatesToolTestServer(t, daemon.URL, true),
				"create_production_reproduction", args)
			require.Equal(t, true, result["isError"])
			require.Equal(t, scenario.code, objectField(t, result, "structuredContent")["code"])
			encoded, err := json.Marshal(result)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "Synthetic private context.")
			require.Equal(t, 1, calls)
		})
	}
}
