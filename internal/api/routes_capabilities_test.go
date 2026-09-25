package api_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestCapabilitiesIdentifiesVaultWithoutHostDetails(t *testing.T) {
	ts, catalog := newTestServer(t, nil)

	resp, body := get(t, ts, "/api/v1/capabilities", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got api.Capabilities
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	assert.Equal(t, catalog.VaultID(), got.VaultUID)
	assert.Equal(t, api.RemoteAPIVersion, got.APIVersion)
	assert.NotEmpty(t, got.Operations)
	assert.NotNil(t, got.Limits)
	assert.NotContains(t, body, catalog.DBPath)
	assert.NotContains(t, body, catalog.BlobsDir)
}

func TestCapabilitiesRequiresDaemonCredential(t *testing.T) {
	ts, _ := newTestServer(t, nil)

	resp, body := get(t, ts, "/api/v1/capabilities", map[string]string{"X-Api-Key": ""})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.NotContains(t, body, testAPIKey)
}
