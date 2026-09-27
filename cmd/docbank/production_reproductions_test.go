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
	"go.kenn.io/docbank/internal/production"
)

func TestProductionReproductionCLIUsesExactDaemonReceipt(t *testing.T) {
	jobID := "82000000-0000-4000-8000-000000000001"
	policy := production.PackageDeliveryPolicy{RecipientCode: "synthetic-recipient",
		AllowedMethods: []string{"offline-media"}}
	policySHA256, err := production.PackageDeliveryPolicySHA256(policy)
	require.NoError(t, err)
	request := documentproduction.ReproductionRequest{
		Contract:                        documentproduction.ReproductionRequestContractV1,
		OperationID:                     "82000000-0000-4000-8000-000000000002",
		OriginalProductionReceiptSHA256: strings.Repeat("a", 64),
		ArtifactIDs:                     []string{"82000000-0000-4000-8000-000000000003"},
		SourceVersionIDs:                []string{"82000000-0000-4000-8000-000000000004"},
		DeliveryPolicySHA256:            policySHA256,
	}
	input := api.ProductionReproductionCreateRequest{
		Request: request, DeliveryPolicy: api.ProductionReproductionDeliveryPolicy(policy),
		ProfileID: "export-dat-opt-images-v1", MaxVolumeBytes: 50 << 20,
		MaxVolumeDocuments: 10,
	}
	fileInput := input
	fileInput.Request.DeliveryPolicySHA256 = ""
	file := filepath.Join(t.TempDir(), "reproduction.json")
	raw, err := json.Marshal(fileInput)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, raw, 0o600))
	receipt := documentproduction.ReproductionReceipt{
		Contract: documentproduction.ReproductionReceiptContractV1, ID: request.OperationID,
		OriginalProductionReceiptSHA256: request.OriginalProductionReceiptSHA256,
		OriginalNumberReservationSHA256: strings.Repeat("b", 64),
		ArtifactManifestSHA256:          strings.Repeat("c", 64),
		PackageQCSHA256:                 strings.Repeat("d", 64),
		DeliveryPolicySHA256:            policySHA256, CreatedAt: "2026-09-27T00:00:00Z",
	}
	_, receipt.SHA256, err = documentproduction.CanonicalReproductionReceipt(receipt)
	require.NoError(t, err)
	var creates, reads int
	path := "/api/v1/productions/jobs/" + jobID + "/reproductions"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "synthetic-api-key", r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case path:
			creates++
			assert.Equal(t, http.MethodPost, r.Method)
			var actual api.ProductionReproductionCreateRequest
			assert.NoError(t, json.UnmarshalRead(r.Body, &actual))
			assert.Equal(t, input, actual)
			w.WriteHeader(http.StatusCreated)
		case path + "/" + request.OperationID:
			reads++
			assert.Equal(t, http.MethodGet, r.Method)
		default:
			assert.Fail(t, "unexpected path", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		assert.NoError(t, json.MarshalWrite(w, receipt))
	}))
	t.Cleanup(server.Close)
	ensureCalls := 0
	ensure := func(context.Context) (*daemonconn.Connection, error) {
		ensureCalls++
		return daemonconn.New(server.URL, "synthetic-api-key"), nil
	}
	create := newProductionReproductionCreateCommandWithEnsure(ensure)
	var created bytes.Buffer
	create.SetOut(&created)
	create.SetArgs([]string{jobID, "--file", file, "--json"})
	require.NoError(t, create.ExecuteContext(t.Context()))
	var decoded documentproduction.ReproductionReceipt
	require.NoError(t, json.Unmarshal(created.Bytes(), &decoded))
	require.Equal(t, receipt, decoded)
	show := newProductionReproductionShowCommandWithEnsure(ensure)
	var shown bytes.Buffer
	show.SetOut(&shown)
	show.SetArgs([]string{jobID, request.OperationID, "--json"})
	require.NoError(t, show.ExecuteContext(t.Context()))
	require.NoError(t, json.Unmarshal(shown.Bytes(), &decoded))
	require.Equal(t, receipt, decoded)
	require.Equal(t, 1, creates)
	require.Equal(t, 1, reads)
	require.Equal(t, 2, ensureCalls)
	input.DeliveryPolicy.RecipientCode = "wrong-recipient"
	raw, err = json.Marshal(input)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, raw, 0o600))
	invalid := newProductionReproductionCreateCommandWithEnsure(ensure)
	invalid.SetArgs([]string{jobID, "--file", file})
	require.Error(t, invalid.ExecuteContext(t.Context()))
	require.Equal(t, 2, ensureCalls)
}
