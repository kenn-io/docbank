package api_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/agentops"
)

func TestAgentCapabilitiesPublishLiveReviewedOperationsSeparatelyFromRemoteTarget(t *testing.T) {
	ts, catalog := newTestServer(t, nil)
	response, body := get(t, ts, "/api/v1/agent/capabilities", nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var capabilities agentops.Capabilities
	require.NoError(t, json.Unmarshal([]byte(body), &capabilities))
	assert.Equal(t, agentops.Schema, capabilities.Schema)
	assert.Equal(t, catalog.VaultID(), capabilities.Server.VaultID)
	assert.NotEmpty(t, capabilities.Server.RegistryDigest)
	assert.NotEmpty(t, capabilities.Server.Operations)
	assert.Equal(t, "no-store", response.Header.Get("Cache-Control"))
	features := make(map[string]agentops.Feature)
	for _, feature := range capabilities.Server.Features {
		features[feature.Name] = feature
	}
	assert.Equal(t, "available", features["native_documents"].State)
	assert.Equal(t, "unavailable", features["native_processing"].State)
	assert.NotEmpty(t, features["native_processing"].Reason)
	for _, operation := range capabilities.Server.Operations {
		assert.True(t, operation.ReviewedBehavior(), operation.ID)
	}
	assert.NotContains(t, body, catalog.DBPath)
	assert.NotContains(t, body, catalog.BlobsDir)

	response, body = get(t, ts, "/api/v1/capabilities", nil)
	require.Equal(t, http.StatusOK, response.StatusCode)
	assert.Contains(t, body, "vault_uid")
	assert.NotContains(t, body, "registry_digest")
}

func TestAgentCapabilitiesRequireLiveScopeAndHideInaccessibleOperations(t *testing.T) {
	fixture := newScopedTagPolicyServer(t)
	response, body := get(t, fixture.server, "/api/v1/agent/capabilities", scopedTagHeaders("first"))
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var capabilities agentops.Capabilities
	require.NoError(t, json.Unmarshal([]byte(body), &capabilities))
	var foundTags bool
	for _, operation := range capabilities.Server.Operations {
		if operation.ID == "list_tags" {
			foundTags = true
		}
		assert.NotEqual(t, "export_job_start", operation.ID)
		assert.NotEqual(t, "get_document", operation.ID)
	}
	assert.True(t, foundTags)
	visibleRoutes := make(map[string]bool)
	for _, route := range capabilities.Server.Routes {
		assert.False(t, route.OperatorOnly)
		visibleRoutes[route.ID] = true
	}
	for _, operation := range capabilities.Server.Operations {
		for _, id := range operation.RouteIDs {
			assert.True(t, visibleRoutes[id], "operation %s refers to hidden route %s", operation.ID, id)
		}
	}

	response, _ = get(t, fixture.server, "/api/v1/agent/capabilities", scopedTagHeaders("unknown"))
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
}
