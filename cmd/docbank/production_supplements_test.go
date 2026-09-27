package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/production"
)

func TestProductionSupplementCLIUsesExactDaemonRecord(t *testing.T) {
	request := production.SupplementRequest{
		OperationID:         "81000000-0000-4000-8000-000000000001",
		ParentJobID:         "81000000-0000-4000-8000-000000000002",
		JobID:               "81000000-0000-4000-8000-000000000003",
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
		SetID:         "81000000-0000-4000-8000-000000000004", Revision: 2,
		NamespaceID:             "81000000-0000-4000-8000-000000000005",
		ParentAllocationID:      "81000000-0000-4000-8000-000000000006",
		AllocationID:            "81000000-0000-4000-8000-000000000007",
		NumberReservationSHA256: strings.Repeat("d", 64),
		ParentEndSequence:       10, StartSequence: 11, EndSequence: 12,
		CreatedAt: "2026-09-27T00:00:00Z",
	}
	_, record.SHA256, err = production.CanonicalSupplementRecord(record)
	require.NoError(t, err)
	var creates, reads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "synthetic-api-key", r.Header.Get("X-Api-Key"))
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
			assert.Fail(t, "unexpected path", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		assert.NoError(t, json.MarshalWrite(w, api.ProductionSupplementRecord(record)))
	}))
	t.Cleanup(server.Close)
	ensure := func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(server.URL, "synthetic-api-key"), nil
	}
	create := newProductionSupplementCreateCommandWithEnsure(ensure)
	var created bytes.Buffer
	create.SetOut(&created)
	create.SetArgs([]string{request.ParentJobID, request.JobID,
		"--operation-id", request.OperationID,
		"--parent-receipt-sha256", request.ParentReceiptSHA256,
		"--prepared-sha256", request.PreparedSHA256,
		"--prepared-input-sha256", request.PreparedInputSHA256, "--json"})
	require.NoError(t, create.ExecuteContext(t.Context()))
	var decoded production.SupplementRecord
	require.NoError(t, json.Unmarshal(created.Bytes(), &decoded))
	require.Equal(t, record, decoded)
	show := newProductionSupplementShowCommandWithEnsure(ensure)
	var shown bytes.Buffer
	show.SetOut(&shown)
	show.SetArgs([]string{request.OperationID, "--json"})
	require.NoError(t, show.ExecuteContext(t.Context()))
	require.NoError(t, json.Unmarshal(shown.Bytes(), &decoded))
	require.Equal(t, record, decoded)
	require.Equal(t, 1, creates)
	require.Equal(t, 1, reads)
}
