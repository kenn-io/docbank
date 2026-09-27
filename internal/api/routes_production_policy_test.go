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

func TestProductionPolicyListBoundsAndCursor(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	policy := documentproduction.PolicyVersion{
		Contract: documentproduction.PolicyContractV1,
		ID:       "11111111-1111-4111-8111-111111111111", Version: 1,
		Name: "Synthetic list policy", CreatedAt: "2026-09-22T13:00:00Z",
		Rules: []documentproduction.PolicyRule{{
			ID: "withhold-selected", Kind: documentproduction.PolicyRuleDisposition,
			Predicate:   documentproduction.PolicyPredicate{Field: "member.id", Operator: documentproduction.PolicyOperatorPresent},
			Disposition: documentproduction.PolicyDispositionWithhold,
		}}, ConflictMode: documentproduction.PolicyConflictReject,
	}
	for version, operationID := range []string{
		"22222222-2222-4222-8222-222222222221",
		"22222222-2222-4222-8222-222222222222",
		"22222222-2222-4222-8222-222222222223",
	} {
		policy.Version = int64(version + 1)
		response, body := do(t, ts, http.MethodPost, "/api/v1/productions/policies", nil,
			map[string]any{"operation_id": operationID, "policy": policy})
		require.Equal(t, http.StatusCreated, response.StatusCode, body)
	}
	policy.ID = "33333333-3333-4333-8333-333333333333"
	policy.Version = 1
	response, body := do(t, ts, http.MethodPost, "/api/v1/productions/policies", nil,
		map[string]any{"operation_id": "44444444-4444-4444-8444-444444444444", "policy": policy})
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/productions/sets", nil,
		map[string]any{"operation_id": "66666666-6666-4666-8666-666666666666", "name": "Synthetic production",
			"instructions": "Synthetic review instructions"})
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	policy.ID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	response, body = do(t, ts, http.MethodPost, "/api/v1/productions/policies", nil,
		map[string]any{"operation_id": "55555555-5555-4555-8555-555555555555", "policy": policy})
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	type policyPage struct {
		Items      []documentproduction.PolicyVersion `json:"items"`
		NextCursor string                             `json:"next_cursor"`
	}
	response, body = get(t, ts, "/api/v1/productions/policies?limit=1", nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var first policyPage
	require.NoError(t, json.Unmarshal([]byte(body), &first))
	require.Len(t, first.Items, 1)
	require.EqualValues(t, 1, first.Items[0].Version)
	require.Equal(t, documentproduction.GenericPolicyID, first.Items[0].ID)
	require.NotEmpty(t, first.NextCursor)
	client := daemonconn.New(ts.URL, testAPIKey)
	clientFirst, err := client.ProductionPolicyVersions(t.Context(), "", 1)
	require.NoError(t, err)
	require.Equal(t, first.NextCursor, clientFirst.NextCursor)
	require.Equal(t, first.Items[0].SHA256, clientFirst.Items[0].SHA256)

	response, body = get(t, ts, "/api/v1/productions/policies?limit=1&cursor="+first.NextCursor, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var second policyPage
	require.NoError(t, json.Unmarshal([]byte(body), &second))
	require.Len(t, second.Items, 1)
	require.EqualValues(t, 1, second.Items[0].Version)
	require.NotEqual(t, first.NextCursor, second.NextCursor)
	clientSecond, err := client.ProductionPolicyVersions(t.Context(), first.NextCursor, 1)
	require.NoError(t, err)
	require.Equal(t, second.Items[0].SHA256, clientSecond.Items[0].SHA256)

	response, body = get(t, ts, "/api/v1/productions/policies?limit=1&cursor="+second.NextCursor, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var third policyPage
	require.NoError(t, json.Unmarshal([]byte(body), &third))
	require.Len(t, third.Items, 1)
	require.EqualValues(t, 2, third.Items[0].Version)
	require.NotEmpty(t, third.NextCursor)
	response, body = get(t, ts, "/api/v1/productions/policies?limit=1&cursor="+third.NextCursor, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var fourth policyPage
	require.NoError(t, json.Unmarshal([]byte(body), &fourth))
	require.Len(t, fourth.Items, 1)
	require.EqualValues(t, 3, fourth.Items[0].Version)
	require.NotEmpty(t, fourth.NextCursor)
	response, body = get(t, ts, "/api/v1/productions/policies?limit=1&cursor="+fourth.NextCursor, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var fifth policyPage
	require.NoError(t, json.Unmarshal([]byte(body), &fifth))
	require.Len(t, fifth.Items, 1)
	require.Equal(t, "33333333-3333-4333-8333-333333333333", fifth.Items[0].ID)
	require.NotEmpty(t, fifth.NextCursor)
	response, body = get(t, ts, "/api/v1/productions/policies?limit=1&cursor="+fifth.NextCursor, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var sixth policyPage
	require.NoError(t, json.Unmarshal([]byte(body), &sixth))
	require.Len(t, sixth.Items, 1)
	require.Equal(t, policy.ID, sixth.Items[0].ID)
	require.Empty(t, sixth.NextCursor)

	response, body = get(t, ts, "/api/v1/productions/policies?limit=101", nil)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	response, body = get(t, ts, "/api/v1/productions/policies?cursor=bad", nil)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
}
