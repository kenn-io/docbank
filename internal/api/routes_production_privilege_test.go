package api_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/productiontest"
)

func TestProductionPrivilegeReadPagesPublicFrozenRows(t *testing.T) {
	const logID = "13131313-1313-4313-8313-131313131313"
	ts, _ := newTestServer(t, func(d *api.Deps) { productiontest.SeedFrozenPrivilegeLog(t, d.Store) })
	response, body := get(t, ts, "/api/v1/production-privilege-logs/"+logID+"?revision=1&limit=1", nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page struct {
		Receipt    documentproduction.PrivilegeLogReceipt  `json:"receipt"`
		Rows       []documentproduction.PrivilegePublicRow `json:"rows"`
		NextCursor string                                  `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Equal(t, logID, page.Receipt.LogID)
	require.Len(t, page.Rows, 1)
	require.Equal(t, "Synthetic public description.", page.Rows[0].PublicDescription)
	require.Equal(t, "1", page.NextCursor)
	client := daemonconn.New(ts.URL, testAPIKey)
	clientPage, err := client.ProductionPrivilegeLog(t.Context(), logID, 1, "", 1)
	require.NoError(t, err)
	require.Equal(t, page.Receipt.SHA256, clientPage.Receipt.SHA256)
	require.Equal(t, page.Rows, clientPage.Rows)
	response, body = get(t, ts, "/api/v1/production-privilege-logs/"+logID+"?revision=1&limit=1&cursor="+page.NextCursor, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var second api.ProductionPrivilegePublicPage
	require.NoError(t, json.Unmarshal([]byte(body), &second))
	require.Len(t, second.Rows, 1)
	require.Equal(t, "Second synthetic public description.", second.Rows[0].PublicDescription)
	require.Empty(t, second.NextCursor)
	clientSecond, err := client.ProductionPrivilegeLog(t.Context(), logID, 1, page.NextCursor, 1)
	require.NoError(t, err)
	require.Equal(t, second.Rows, clientSecond.Rows)
	for _, private := range []string{"Synthetic private rationale.", "person_ids", "evidence_sha256", "fields"} {
		require.NotContains(t, body, private)
	}
	response, body = get(t, ts, "/api/v1/production-privilege-logs/"+logID+"?revision=1&limit=101", nil)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	response, body = get(t, ts, "/api/v1/production-privilege-logs/"+logID+"?revision=1&cursor=01", nil)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	response, body = get(t, ts, "/api/v1/production-privilege-logs/"+logID+"?revision=2", nil)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
}

func TestProductionPrivilegeValidationUsesStoredRowsAndReplays(t *testing.T) {
	var draft productiontest.PrivilegeDraft
	ts, _ := newTestServer(t, func(d *api.Deps) { draft = productiontest.SeedPrivilegeLogDraft(t, d.Store) })
	path := "/api/v1/production-privilege-logs/" + draft.LogID + "/revisions/1/validate"
	const operationID = "edededed-eded-4ded-8ded-edededededed"
	const validatedAt = "2026-09-22T14:00:00Z"
	request := map[string]any{
		"operation_id":        operationID,
		"expected_generation": draft.Generation,
		"validated_at":        validatedAt,
	}
	response, body := do(t, ts, http.MethodPost, path, nil, request)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var validation struct {
		DraftGeneration int64                                     `json:"draft_generation"`
		Validation      documentproduction.PrivilegeLogValidation `json:"validation"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &validation))
	require.Equal(t, draft.Generation, validation.DraftGeneration)
	require.Equal(t, draft.LogID, validation.Validation.Inputs.LogID)
	require.NotEmpty(t, validation.Validation.InputsSHA256)
	client := daemonconn.New(ts.URL, testAPIKey)
	clientValidation, err := client.ValidateProductionPrivilegeLog(t.Context(), draft.LogID, draft.Revision,
		api.ProductionPrivilegeValidationRequest{OperationID: operationID,
			ExpectedGeneration: draft.Generation, ValidatedAt: validatedAt})
	require.NoError(t, err)
	require.Equal(t, validation.Validation.InputsSHA256, clientValidation.Validation.InputsSHA256)
	for _, private := range []string{"Synthetic private rationale.", "person_ids", "evidence_sha256", "fields"} {
		require.NotContains(t, body, private)
	}
	response, replay := do(t, ts, http.MethodPost, path, nil, request)
	require.Equal(t, http.StatusCreated, response.StatusCode, replay)
	require.Equal(t, body, replay)
	request["expected_generation"] = draft.Generation + 1
	response, body = do(t, ts, http.MethodPost, path, nil, request)
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	request["operation_id"] = "fefefefe-fefe-4efe-8efe-fefefefefefe"
	response, body = do(t, ts, http.MethodPost, path, nil, request)
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	request["expected_generation"] = draft.Generation
	request["validated_at"] = "2026-09-22T14:00:00+00:00"
	response, body = do(t, ts, http.MethodPost, path, nil, request)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	require.NotContains(t, body, "Synthetic private rationale.")
	request["validated_at"] = "2026-09-22T14:00:01Z"
	response, body = do(t, ts, http.MethodPost,
		"/api/v1/production-privilege-logs/abababab-abab-4bab-8bab-abababababab/revisions/1/validate",
		nil, request)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
}
