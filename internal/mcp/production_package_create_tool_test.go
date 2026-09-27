package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/production"
)

func syntheticPackageToolEvidence(t *testing.T) (string, api.ProductionPackageCreateRequest,
	production.PackageEvidenceReceipt) {
	t.Helper()
	jobID := "85000000-0000-4000-8000-000000000001"
	input := api.ProductionPackageCreateRequest{
		OperationID: "85000000-0000-4000-8000-000000000002",
		ProfileID:   "export-dat-opt-images-v1", MaxVolumeBytes: 50 << 20,
		MaxVolumeDocuments: 10,
	}
	evidence := production.PackageEvidenceReceipt{
		Contract: production.PackageEvidenceContractV1, ID: input.OperationID, JobID: jobID,
		ProductionReceiptSHA256: strings.Repeat("a", 64),
		ArtifactManifestSHA256:  strings.Repeat("b", 64),
		RecipientManifestSHA256: strings.Repeat("c", 64),
		ArchiveSHA256:           strings.Repeat("d", 64), QCSHA256: strings.Repeat("e", 64),
		ProfileID: input.ProfileID,
	}
	encoded, err := canonical.Marshal(evidence)
	require.NoError(t, err)
	digest := sha256.Sum256(encoded)
	evidence.SHA256 = hex.EncodeToString(digest[:])
	require.NoError(t, production.ValidatePackageEvidenceReceipt(evidence))
	return jobID, input, evidence
}

func TestProductionPackageToolsCreateAndReadExactEvidence(t *testing.T) {
	jobID, input, evidence := syntheticPackageToolEvidence(t)
	require.Nil(t, catalogMap(toolCatalog(false))["create_production_package"])
	require.NotNil(t, catalogMap(toolCatalog(false))["get_production_package"])
	require.NotNil(t, catalogMap(toolCatalog(true))["create_production_package"])
	var creates, reads int
	path := "/api/v1/productions/jobs/" + jobID + "/packages"
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case path:
			creates++
			assert.Equal(t, http.MethodPost, r.Method)
			var actual api.ProductionPackageCreateRequest
			assert.NoError(t, json.UnmarshalRead(r.Body, &actual))
			assert.Equal(t, input, actual)
			w.WriteHeader(http.StatusCreated)
		case path + "/" + input.OperationID:
			reads++
			assert.Equal(t, http.MethodGet, r.Method)
		default:
			http.NotFound(w, r)
			return
		}
		assert.NoError(t, json.MarshalWrite(w, api.ProductionPackageEvidenceReceipt(evidence)))
	}))
	t.Cleanup(daemon.Close)
	args := map[string]any{
		"job_id": jobID, "operation_id": input.OperationID, "profile_id": input.ProfileID,
		"max_volume_bytes": input.MaxVolumeBytes, "max_volume_documents": input.MaxVolumeDocuments,
	}
	encodedArgs, err := json.Marshal(args)
	require.NoError(t, err)
	var schemaArgs map[string]any
	require.NoError(t, json.Unmarshal(encodedArgs, &schemaArgs))
	assertSchemaAccepts(t, catalogMap(toolCatalog(true))["create_production_package"].InputSchema, schemaArgs)
	created := callToolResult(t, newBatesToolTestServer(t, daemon.URL, true), "create_production_package", args)
	require.NotEqual(t, true, created["isError"], created)
	require.Equal(t, evidence.SHA256, objectField(t, objectField(t, created, "structuredContent"), "evidence")["sha256"])
	shown := callToolResult(t, newBatesToolTestServer(t, daemon.URL, false), "get_production_package",
		map[string]any{"job_id": jobID, "operation_id": input.OperationID})
	require.NotEqual(t, true, shown["isError"], shown)
	require.Equal(t, evidence.SHA256, objectField(t, objectField(t, shown, "structuredContent"), "evidence")["sha256"])
	require.Equal(t, 1, creates)
	require.Equal(t, 1, reads)
}

func TestProductionPackageToolPreservesConflictAndUnknownOutcome(t *testing.T) {
	jobID, input, _ := syntheticPackageToolEvidence(t)
	args := map[string]any{
		"job_id": jobID, "operation_id": input.OperationID, "profile_id": input.ProfileID,
		"max_volume_bytes": input.MaxVolumeBytes, "max_volume_documents": input.MaxVolumeDocuments,
	}
	for _, scenario := range []struct {
		name, code string
		status     int
		body       string
	}{
		{"conflict", "production_package_conflict", http.StatusConflict,
			`{"status":409,"code":"production_package_conflict","detail":"Synthetic private context."}`},
		{"uncertain response", "production_package_outcome_unknown", http.StatusCreated, "{"},
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
			result := callToolResult(t, newBatesToolTestServer(t, daemon.URL, true), "create_production_package", args)
			require.Equal(t, true, result["isError"])
			require.Equal(t, scenario.code, objectField(t, result, "structuredContent")["code"])
			encoded, err := json.Marshal(result)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "Synthetic private context.")
			require.Equal(t, 1, calls)
		})
	}
}
