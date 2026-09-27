package api_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestProductionPolicyCreateReadAndReplay(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	policy := documentproduction.PolicyVersion{
		Contract: documentproduction.PolicyContractV1,
		ID:       "11111111-1111-4111-8111-111111111111", Version: 1,
		Name: "Synthetic disclosure policy", CreatedAt: "2026-09-22T13:00:00Z",
		Rules: []documentproduction.PolicyRule{{
			ID: "withhold-selected", Kind: documentproduction.PolicyRuleDisposition,
			Predicate:   documentproduction.PolicyPredicate{Field: "member.id", Operator: documentproduction.PolicyOperatorPresent},
			Disposition: documentproduction.PolicyDispositionWithhold,
		}},
		ConflictMode: documentproduction.PolicyConflictReject,
	}
	const operationID = "22222222-2222-4222-8222-222222222222"
	request := map[string]any{"operation_id": operationID, "policy": policy}
	response, body := do(t, ts, http.MethodPost, "/api/v1/productions/policies", nil, request)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var created documentproduction.PolicyVersion
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	require.NotEmpty(t, created.SHA256)
	require.NoError(t, documentproduction.ValidatePolicyVersion(created))

	response, body = do(t, ts, http.MethodPost, "/api/v1/productions/policies", nil, request)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var replay documentproduction.PolicyVersion
	require.NoError(t, json.Unmarshal([]byte(body), &replay))
	require.Equal(t, created, replay)

	response, body = get(t, ts, "/api/v1/productions/policies/"+policy.ID+"/versions/1", nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var read documentproduction.PolicyVersion
	require.NoError(t, json.Unmarshal([]byte(body), &read))
	require.Equal(t, created.SHA256, read.SHA256)
	require.NoError(t, documentproduction.ValidatePolicyVersion(read))
	client := daemonconn.New(ts.URL, testAPIKey)
	clientRead, err := client.ProductionPolicyVersion(t.Context(), policy.ID, policy.Version)
	require.NoError(t, err)
	require.Equal(t, created.SHA256, clientRead.SHA256)
	clientReplay, err := client.CreateProductionPolicyVersion(t.Context(), api.ProductionPolicyCreateRequest{
		OperationID: operationID, Policy: policy})
	require.NoError(t, err)
	require.Equal(t, created.SHA256, clientReplay.SHA256)

	other := policy
	other.Name = "Altered policy"
	changed := map[string]any{"operation_id": operationID, "policy": other}
	response, body = do(t, ts, http.MethodPost, "/api/v1/productions/policies", nil, changed)
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	changed["operation_id"] = "33333333-3333-4333-8333-333333333333"
	response, body = do(t, ts, http.MethodPost, "/api/v1/productions/policies", nil, changed)
	require.Equal(t, http.StatusConflict, response.StatusCode, body)

	withDigest := policy
	withDigest.SHA256 = created.SHA256
	response, body = do(t, ts, http.MethodPost, "/api/v1/productions/policies", nil,
		map[string]any{"operation_id": "44444444-4444-4444-8444-444444444444", "policy": withDigest})
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)

	response, body = get(t, ts, "/api/v1/productions/policies/"+policy.ID+"/versions/2", nil)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
}
