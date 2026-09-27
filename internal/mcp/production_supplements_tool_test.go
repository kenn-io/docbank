package mcp

import (
	"encoding/json/v2"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/production"
)

func syntheticSupplementToolRecord(t *testing.T) (production.SupplementRequest, production.SupplementRecord) {
	t.Helper()
	request := production.SupplementRequest{
		OperationID:         "82000000-0000-4000-8000-000000000001",
		ParentJobID:         "82000000-0000-4000-8000-000000000002",
		JobID:               "82000000-0000-4000-8000-000000000003",
		ParentReceiptSHA256: strings.Repeat("a", 64),
		PreparedSHA256:      strings.Repeat("b", 64),
		PreparedInputSHA256: strings.Repeat("c", 64),
	}
	requestSHA256, err := production.SupplementRequestSHA256(request)
	require.NoError(t, err)
	record := production.SupplementRecord{
		Contract:    production.SupplementRecordContractV1,
		OperationID: request.OperationID, ParentJobID: request.ParentJobID, JobID: request.JobID,
		ParentReceiptSHA256: request.ParentReceiptSHA256,
		PreparedSHA256:      request.PreparedSHA256, PreparedInputSHA256: request.PreparedInputSHA256,
		RequestSHA256: requestSHA256,
		SetID:         "82000000-0000-4000-8000-000000000004", Revision: 2,
		NamespaceID:             "82000000-0000-4000-8000-000000000005",
		ParentAllocationID:      "82000000-0000-4000-8000-000000000006",
		AllocationID:            "82000000-0000-4000-8000-000000000007",
		NumberReservationSHA256: strings.Repeat("d", 64),
		ParentEndSequence:       10, StartSequence: 11, EndSequence: 12,
		CreatedAt: "2026-09-27T00:00:00Z",
	}
	_, record.SHA256, err = production.CanonicalSupplementRecord(record)
	require.NoError(t, err)
	return request, record
}

func TestProductionSupplementToolsCreateAndReadExactRecord(t *testing.T) {
	request, record := syntheticSupplementToolRecord(t)
	require.Nil(t, catalogMap(toolCatalog(false))["create_production_supplement"])
	require.NotNil(t, catalogMap(toolCatalog(false))["get_production_supplement"])
	require.NotNil(t, catalogMap(toolCatalog(true))["create_production_supplement"])
	var creates, reads int
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/productions/supplements":
			creates++
			assert.Equal(t, http.MethodPost, r.Method)
			var actual api.ProductionSupplementRequest
			assert.NoError(t, json.UnmarshalRead(r.Body, &actual))
			assert.Equal(t, request, production.SupplementRequest(actual))
			w.WriteHeader(http.StatusCreated)
		case "/api/v1/productions/supplements/" + request.OperationID:
			reads++
			assert.Equal(t, http.MethodGet, r.Method)
		default:
			http.NotFound(w, r)
			return
		}
		assert.NoError(t, json.MarshalWrite(w, api.ProductionSupplementRecord(record)))
	}))
	t.Cleanup(daemon.Close)
	writes := newBatesToolTestServer(t, daemon.URL, true)
	args := map[string]any{
		"operation_id": request.OperationID, "parent_job_id": request.ParentJobID,
		"job_id": request.JobID, "parent_receipt_sha256": request.ParentReceiptSHA256,
		"prepared_sha256":       request.PreparedSHA256,
		"prepared_input_sha256": request.PreparedInputSHA256,
	}
	assertSchemaAccepts(t, catalogMap(toolCatalog(true))["create_production_supplement"].InputSchema, args)
	missingDigest := map[string]any{}
	maps.Copy(missingDigest, args)
	delete(missingDigest, "prepared_input_sha256")
	assertSchemaRejects(t, catalogMap(toolCatalog(true))["create_production_supplement"].InputSchema, missingDigest)
	first := callToolResult(t, writes, "create_production_supplement", args)
	require.NotEqual(t, true, first["isError"], first)
	summary := objectField(t, objectField(t, first, "structuredContent"), "record")
	require.Equal(t, record.SHA256, summary["sha256"])
	require.EqualValues(t, record.StartSequence, summary["start_sequence"])
	readOnly := newBatesToolTestServer(t, daemon.URL, false)
	shown := callToolResult(t, readOnly, "get_production_supplement", map[string]any{"operation_id": request.OperationID})
	require.NotEqual(t, true, shown["isError"], shown)
	require.Equal(t, record.SHA256, objectField(t, objectField(t, shown, "structuredContent"), "record")["sha256"])
	require.Equal(t, 1, creates)
	require.Equal(t, 1, reads)
}

func TestProductionSupplementToolPreservesConflictAndUnknownOutcome(t *testing.T) {
	request, _ := syntheticSupplementToolRecord(t)
	args := map[string]any{
		"operation_id": request.OperationID, "parent_job_id": request.ParentJobID,
		"job_id": request.JobID, "parent_receipt_sha256": request.ParentReceiptSHA256,
		"prepared_sha256":       request.PreparedSHA256,
		"prepared_input_sha256": request.PreparedInputSHA256,
	}
	for _, scenario := range []struct {
		name, code string
		status     int
		body       string
	}{
		{"conflict", "production_supplement_conflict", http.StatusConflict,
			`{"status":409,"code":"production_supplement_conflict","detail":"Synthetic private context."}`},
		{"uncertain response", "production_supplement_outcome_unknown", http.StatusCreated, "{"},
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
			server := newBatesToolTestServer(t, daemon.URL, true)
			result := callToolResult(t, server, "create_production_supplement", args)
			require.Equal(t, true, result["isError"])
			require.Equal(t, scenario.code, objectField(t, result, "structuredContent")["code"])
			encoded, err := json.Marshal(result)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "Synthetic private context.")
			require.Equal(t, 1, calls)
		})
	}
}
