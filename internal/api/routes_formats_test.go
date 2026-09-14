package api_test

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
)

func TestFormatCapabilitiesFiltersAndLooksUp(t *testing.T) {
	ts, _ := newTestServer(t, nil)

	resp, body := get(t, ts, "/api/v1/formats/capabilities", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var topLevel map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &topLevel))
	assert.Contains(t, topLevel, "contract_version")
	assert.Contains(t, topLevel, "formats")
	assert.Contains(t, topLevel, "pending")
	assert.Contains(t, topLevel, "generated_by")
	assert.NotContains(t, topLevel, "coverage")
	var all api.FormatCoverageResponse
	require.NoError(t, json.Unmarshal([]byte(body), &all))
	assert.Equal(t, document.FormatCoverageContractV1, all.ContractVersion)
	require.Len(t, all.Formats, len(document.FormatMetadataCatalog()))
	assert.Nil(t, all.Lookup)

	resp, body = get(t, ts, "/api/v1/formats/capabilities?family=archive", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var archives api.FormatCoverageResponse
	require.NoError(t, json.Unmarshal([]byte(body), &archives))
	require.NotEmpty(t, archives.Formats)
	for _, format := range archives.Formats {
		assert.Equal(t, "archive", format.QueryFamily)
	}

	resp, body = get(t, ts, "/api/v1/formats/capabilities?format=zip", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var zip api.FormatCoverageResponse
	require.NoError(t, json.Unmarshal([]byte(body), &zip))
	require.Len(t, zip.Formats, 1)
	require.NotNil(t, zip.Lookup)
	assert.Equal(t, document.FormatLookupFormat, zip.Lookup.Match)
	assert.Equal(t, "zip", zip.Lookup.Format.ID)

	resp, body = get(t, ts, "/api/v1/formats/capabilities?extension=wpd", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var pending api.FormatCoverageResponse
	require.NoError(t, json.Unmarshal([]byte(body), &pending))
	require.NotNil(t, pending.Lookup)
	assert.Equal(t, document.FormatLookupPending, pending.Lookup.Match)
	require.NotNil(t, pending.Lookup.Pending)
	assert.Equal(t, "DB-42b", pending.Lookup.Pending.OwnerSlice)

	resp, body = get(t, ts, "/api/v1/formats/capabilities?extension=qqq", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var unknown api.FormatCoverageResponse
	require.NoError(t, json.Unmarshal([]byte(body), &unknown))
	require.NotNil(t, unknown.Lookup)
	assert.Equal(t, document.FormatLookupUnknown, unknown.Lookup.Match)
	assert.Equal(t, "qqq", unknown.Lookup.Query)

	resp, body = get(t, ts, "/api/v1/formats/capabilities?format=zip&extension=zip", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	var problem api.Error
	require.NoError(t, json.Unmarshal([]byte(body), &problem))
	assert.Equal(t, "invalid_format_query", problem.Code)
}

func TestFormatCapabilitiesLookupUsesFullSnapshotBeforeFamilyFilter(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, body := get(t, ts,
		"/api/v1/formats/capabilities?family=archive&format=pdf", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got api.FormatCoverageResponse
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	require.NotNil(t, got.Lookup)
	assert.Equal(t, document.FormatLookupFormat, got.Lookup.Match)
	assert.Equal(t, "pdf", got.Lookup.Format.ID)
	for _, format := range got.Formats {
		assert.Equal(t, "archive", format.QueryFamily)
	}
}

func TestFormatCapabilitiesSnapshotIsFrozenAndDeepCopied(t *testing.T) {
	configure := configureProcessingTestService(t)
	var snapshot document.FormatCoverageV1
	ts, _ := newTestServer(t, func(d *api.Deps) {
		configure(d)
		snapshot = d.Processing.FormatCoverage()
	})
	originalCount := len(snapshot.Formats)
	originalState := snapshot.Formats[0].Capabilities[document.CapabilityDetect]
	require.Len(t, snapshot.GeneratedBy.BoundProviders, 1)
	boundProvider := snapshot.GeneratedBy.BoundProviders[0]

	snapshot.Formats[0].Capabilities[document.CapabilityDetect] = document.CapabilityStateV1{
		State: document.CapabilityUnsupported, Note: "mutated after construction",
	}
	snapshot.Formats[0].Extensions[0] = "mutated"
	snapshot.GeneratedBy.BoundProviders = append(snapshot.GeneratedBy.BoundProviders, strings.Repeat("f", 64))

	resp, _ := get(t, ts, "/api/v1/formats/capabilities?family=archive", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp, body := get(t, ts, "/api/v1/formats/capabilities", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got api.FormatCoverageResponse
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	require.Len(t, got.Formats, originalCount)
	assert.Equal(t, originalState, got.Formats[0].Capabilities[document.CapabilityDetect])
	assert.NotContains(t, got.Formats[0].Extensions, "mutated")
	assert.NotContains(t, got.GeneratedBy.BoundProviders, strings.Repeat("f", 64))
	assert.Equal(t, []string{boundProvider}, got.GeneratedBy.BoundProviders)
}

func TestFormatsRouteRequiresAuthentication(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	r, err := http.Get(ts.URL + "/api/v1/formats/capabilities")
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, r.StatusCode)
	require.NoError(t, r.Body.Close())

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/formats/capabilities", nil)
	require.NoError(t, err)
	req.Header.Set("X-Api-Key", testAPIKey)
	r, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, r.StatusCode)
	require.NoError(t, r.Body.Close())
}

func TestFormatsRouteEnforcesSelectorByteLimits(t *testing.T) {
	ts, _ := newTestServer(t, nil)

	resp, _ := get(t, ts, "/api/v1/formats/capabilities?extension="+strings.Repeat("é", 8), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "eight two-byte runes fit the 16-byte limit")

	resp, body := get(t, ts,
		"/api/v1/formats/capabilities?extension="+strings.Repeat("é", 9), nil)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	var problem api.Error
	require.NoError(t, json.Unmarshal([]byte(body), &problem))
	assert.Equal(t, "invalid_format_query", problem.Code)
	assert.Contains(t, problem.Detail, "16 bytes")
}

func TestFormatsRouteBrowserPolicyBoundsSelectors(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var issued struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &issued))

	request := func(rawQuery string) int {
		req, err := http.NewRequest(http.MethodGet,
			ts.URL+"/api/v1/formats/capabilities"+rawQuery, nil)
		require.NoError(t, err)
		req.Header["X-Api-Key"] = []string{""}
		req.Header.Set(api.WebSessionHeader, issued.Token)
		response, err := ts.Client().Do(req)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode
	}

	assert.Equal(t, http.StatusOK, request(""))
	assert.Equal(t, http.StatusOK, request("?family=archive&extension=wpd"))
	assert.Equal(t, http.StatusForbidden, request("?unknown=x"))
	assert.Equal(t, http.StatusForbidden, request("?format=zip&format=pdf"))
	assert.Equal(t, http.StatusForbidden, request("?family="+strings.Repeat("a", 65)))
	assert.Equal(t, http.StatusForbidden, request("?extension="+strings.Repeat("a", 17)))
}
