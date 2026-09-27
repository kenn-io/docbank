package api_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/productiontest"
)

func TestProductionPrivilegeExportClientRejectsAlteredBytes(t *testing.T) {
	const logID = "13131313-1313-4313-8313-131313131313"
	_, fixture := newTestServer(t, func(d *api.Deps) { productiontest.SeedFrozenPrivilegeLog(t, d.Store) })
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorded := httptest.NewRecorder()
		fixture.Server.Handler().ServeHTTP(recorded, r)
		response := recorded.Result()
		defer func() { _ = response.Body.Close() }()
		content, err := io.ReadAll(response.Body)
		if err != nil {
			t.Errorf("read source response: %v", err)
			return
		}
		for key, values := range response.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		if strings.HasSuffix(r.URL.Path, "/exports/csv") {
			content[len(content)-1] = 'X'
		}
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(content)
	}))
	t.Cleanup(proxy.Close)
	client := daemonconn.New(proxy.URL, testAPIKey)
	_, err := client.ExportProductionPrivilegeLog(t.Context(), logID, 1, "csv")
	require.ErrorContains(t, err, "digest")
}

func TestProductionPrivilegeExportServesVerifiedPublicBytes(t *testing.T) {
	const logID = "13131313-1313-4313-8313-131313131313"
	ts, _ := newTestServer(t, func(d *api.Deps) { productiontest.SeedFrozenPrivilegeLog(t, d.Store) })
	for _, test := range []struct{ format, mediaType string }{
		{"json", "application/json"}, {"csv", "text/csv; charset=utf-8"},
		{"xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
		{"pdf", "application/pdf"},
	} {
		t.Run(test.format, func(t *testing.T) {
			response, body := get(t, ts, "/api/v1/production-privilege-logs/"+logID+
				"/revisions/1/exports/"+test.format, nil)
			require.Equal(t, http.StatusOK, response.StatusCode, body)
			require.Equal(t, test.mediaType, response.Header.Get("Content-Type"))
			require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
			require.Equal(t, "nosniff", response.Header.Get("X-Content-Type-Options"))
			require.NotEmpty(t, response.Header.Get("X-Docbank-Privilege-Receipt-Sha256"))
			require.NotEmpty(t, response.Header.Get("X-Docbank-Privilege-Rows-Sha256"))
			digest := sha256.Sum256([]byte(body))
			require.Equal(t, "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":",
				response.Header.Get("Content-Digest"))
			require.NotContains(t, body, "Synthetic private rationale.")
			require.NotContains(t, body, "synthetic@example.test")
			client := daemonconn.New(ts.URL, testAPIKey)
			clientExport, err := client.ExportProductionPrivilegeLog(t.Context(), logID, 1, test.format)
			require.NoError(t, err)
			require.Equal(t, []byte(body), clientExport.Content)
			require.Equal(t, test.mediaType, clientExport.MediaType)
			require.Equal(t, hex.EncodeToString(digest[:]), clientExport.ContentSHA256)
		})
	}
	response, body := get(t, ts, "/api/v1/production-privilege-logs/"+logID+"/revisions/2/exports/csv", nil)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = get(t, ts, "/api/v1/production-privilege-logs/"+logID+"/revisions/0/exports/csv", nil)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	response, body = get(t, ts, "/api/v1/production-privilege-logs/"+logID+"/revisions/1/exports/html", nil)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	response, body = get(t, ts, "/api/v1/production-privilege-logs/"+logID+"/revisions/1/exports/csv",
		map[string]string{"X-Api-Key": ""})
	require.Equal(t, http.StatusUnauthorized, response.StatusCode, body)
}

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
