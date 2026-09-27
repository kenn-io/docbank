package main

import (
	"bytes"
	"context"
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
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/production"
)

func TestProductionPackageCLIUsesExactDaemonEvidence(t *testing.T) {
	jobID := "84000000-0000-4000-8000-000000000001"
	input := api.ProductionPackageCreateRequest{
		OperationID: "84000000-0000-4000-8000-000000000002",
		ProfileID:   "export-dat-opt-images-v1", MaxVolumeBytes: 50 << 20,
		MaxVolumeDocuments: 10,
	}
	evidence := production.PackageEvidenceReceipt{
		Contract: production.PackageEvidenceContractV1,
		ID:       input.OperationID, JobID: jobID,
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
	var creates, reads int
	path := "/api/v1/productions/jobs/" + jobID + "/packages"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "synthetic-api-key", r.Header.Get("X-Api-Key"))
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
			assert.Fail(t, "unexpected package path", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		assert.NoError(t, json.MarshalWrite(w, api.ProductionPackageEvidenceReceipt(evidence)))
	}))
	t.Cleanup(server.Close)
	ensureCalls := 0
	ensure := func(context.Context) (*daemonconn.Connection, error) {
		ensureCalls++
		return daemonconn.New(server.URL, "synthetic-api-key"), nil
	}
	create := newProductionPackageCreateCommandWithEnsure(ensure)
	var created bytes.Buffer
	create.SetOut(&created)
	create.SetArgs([]string{jobID, "--operation-id", input.OperationID,
		"--profile", input.ProfileID, "--max-volume-bytes", "52428800",
		"--max-volume-documents", "10", "--json"})
	require.NoError(t, create.ExecuteContext(t.Context()))
	var decoded production.PackageEvidenceReceipt
	require.NoError(t, json.Unmarshal(created.Bytes(), &decoded))
	require.Equal(t, evidence, decoded)
	show := newProductionPackageShowCommandWithEnsure(ensure)
	var shown bytes.Buffer
	show.SetOut(&shown)
	show.SetArgs([]string{jobID, input.OperationID, "--json"})
	require.NoError(t, show.ExecuteContext(t.Context()))
	require.NoError(t, json.Unmarshal(shown.Bytes(), &decoded))
	require.Equal(t, evidence, decoded)
	require.Equal(t, 1, creates)
	require.Equal(t, 1, reads)
	require.Equal(t, 2, ensureCalls)
	invalid := newProductionPackageCreateCommandWithEnsure(ensure)
	invalid.SetArgs([]string{jobID, "--operation-id", "invalid", "--profile", input.ProfileID})
	require.Error(t, invalid.ExecuteContext(t.Context()))
	require.Equal(t, 2, ensureCalls)
}
