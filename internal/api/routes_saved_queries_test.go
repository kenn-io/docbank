package api_test

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

const fullSavedQueryPayload = `{"v":1,"text":"status:open AND (owner:me OR owner:team)","syntax":"advanced","mode":"hybrid","filters":{"paths":["/records"],"extensions":["md","txt"]},"sort":{"field":"modified_at","direction":"desc"}}`

func rawJSONRequest(
	t *testing.T, tsURL, method, path string, headers map[string]string, body string,
) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, tsURL+path, strings.NewReader(body))
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, string(raw)
}

func createSavedQuery(t *testing.T, baseURL, name, payload string) (api.SavedQuery, string) {
	t.Helper()
	resp, body := rawJSONRequest(t, baseURL, http.MethodPost, "/api/v1/saved-queries",
		map[string]string{"X-Api-Key": testAPIKey}, fmt.Sprintf(
			`{"name":%q,"description":"Synthetic definition","kind":"query","payload":%s}`,
			name, payload,
		))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var saved api.SavedQuery
	require.NoError(t, json.Unmarshal([]byte(body), &saved))
	return saved, resp.Header.Get("ETag")
}

func decodeProblem(t *testing.T, body string) api.Error {
	t.Helper()
	var problem api.Error
	require.NoError(t, json.Unmarshal([]byte(body), &problem))
	return problem
}

func streamedJSONRequest(
	t *testing.T, tsURL, method, path string, headers map[string]string, body string,
) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, tsURL+path, struct{ io.Reader }{strings.NewReader(body)})
	require.NoError(t, err)
	require.Zero(t, req.ContentLength)
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, string(raw)
}

func TestSavedQueryPayloadRejectsOversizeBeforeMutatingReceiver(t *testing.T) {
	payload := api.SavedQueryPayload(`{"text":"preserved"}`)
	before := append(api.SavedQueryPayload(nil), payload...)
	oversized := []byte("{" + strings.Repeat(" ", 128<<10) + "}")

	err := payload.UnmarshalJSON(oversized)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "128 KiB")
	assert.Equal(t, before, payload)
}

func TestSavedQueryHTTPRequestBodyBounds(t *testing.T) {
	const maxRequestBytes = 160 << 10

	t.Run("accepts maximum payload with legal escaped envelope and omitted payload patch", func(t *testing.T) {
		ts, _ := newTestServer(t, nil)
		payload := "{" + strings.Repeat(" ", (128<<10)-2) + "}"
		body := `{"name":"` + strings.Repeat(`\u0100`, 128) +
			`","description":"` + strings.Repeat(`\u0100`, 2048) +
			`","kind":"query","payload":` + payload + `}`
		require.Len(t, []byte(payload), 128<<10)
		require.Less(t, len(body), maxRequestBytes)

		resp, responseBody := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/saved-queries",
			map[string]string{"X-Api-Key": testAPIKey}, body)
		require.Equal(t, http.StatusCreated, resp.StatusCode, responseBody)
		var created api.SavedQuery
		require.NoError(t, json.Unmarshal([]byte(responseBody), &created))

		resp, responseBody = rawJSONRequest(t, ts.URL, http.MethodPatch,
			"/api/v1/saved-queries/"+created.ID,
			map[string]string{"X-Api-Key": testAPIKey, "If-Match": `"1"`},
			`{"name":"Bounded rename"}`)
		require.Equal(t, http.StatusOK, resp.StatusCode, responseBody)
		var patched api.SavedQuery
		require.NoError(t, json.Unmarshal([]byte(responseBody), &patched))
		assert.Equal(t, "Bounded rename", patched.Name)
		assert.EqualValues(t, 2, patched.Revision)
	})

	t.Run("rejects create envelope at limit without adding a definition", func(t *testing.T) {
		ts, s := newTestServer(t, nil)
		created, _ := createSavedQuery(t, ts.URL, "Stable definition", `{}`)
		body := `{"name":"Rejected definition","kind":"query","payload":{}}`
		body += strings.Repeat(" ", maxRequestBytes-len(body))
		require.Len(t, []byte(body), maxRequestBytes)

		resp, responseBody := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/saved-queries",
			map[string]string{"X-Api-Key": testAPIKey}, body)
		assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode, responseBody)
		items, total, err := s.SavedQueries(t.Context(), "", 100, 0)
		require.NoError(t, err)
		assert.Equal(t, 1, total)
		require.Len(t, items, 1)
		assert.Equal(t, created.ID, items[0].ID)
	})

	t.Run("rejects streamed patch envelope at limit without revision mutation", func(t *testing.T) {
		ts, s := newTestServer(t, nil)
		created, _ := createSavedQuery(t, ts.URL, "Stable definition", `{}`)
		body := `{"name":"Rejected rename"}`
		body += strings.Repeat(" ", maxRequestBytes-len(body))
		require.Len(t, []byte(body), maxRequestBytes)

		resp, responseBody := streamedJSONRequest(t, ts.URL, http.MethodPatch,
			"/api/v1/saved-queries/"+created.ID,
			map[string]string{"X-Api-Key": testAPIKey, "If-Match": `"1"`}, body)
		assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode, responseBody)
		after, err := s.SavedQueryByID(t.Context(), created.ID)
		require.NoError(t, err)
		assert.Equal(t, created.Name, after.Name)
		assert.Equal(t, created.Revision, after.Revision)
	})

	t.Run("rejects oversized nested payload without revision mutation", func(t *testing.T) {
		ts, s := newTestServer(t, nil)
		created, _ := createSavedQuery(t, ts.URL, "Stable definition", `{}`)
		payload := "{" + strings.Repeat(" ", (128<<10)-1) + "}"
		require.Len(t, []byte(payload), (128<<10)+1)

		resp, responseBody := rawJSONRequest(t, ts.URL, http.MethodPatch,
			"/api/v1/saved-queries/"+created.ID,
			map[string]string{"X-Api-Key": testAPIKey, "If-Match": `"1"`},
			`{"payload":`+payload+`}`)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, responseBody)
		after, err := s.SavedQueryByID(t.Context(), created.ID)
		require.NoError(t, err)
		assert.Equal(t, string(created.Payload), string(after.Payload))
		assert.Equal(t, created.Revision, after.Revision)
	})
}

func TestSavedQueryHTTPRoundTripPaginationAndFences(t *testing.T) {
	ts, _ := newTestServer(t, nil)

	created, etag := createSavedQuery(t, ts.URL, "Synthetic search", fullSavedQueryPayload)
	assert.NotEmpty(t, created.ID)
	assert.Equal(t, "Synthetic search", created.Name)
	assert.Equal(t, "Synthetic definition", created.Description)
	assert.Equal(t, "query", created.Kind)
	assert.EqualValues(t, 1, created.Revision)
	assert.Equal(t, `"1"`, etag)
	assert.Contains(t, string(created.Payload), `"text":"status:open AND (owner:me OR owner:team)"`)
	assert.NotContains(t, string(created.Payload), "eyJ",
		"payload must be structured JSON rather than base64 bytes")
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, created.Fingerprint)
	assert.NotEmpty(t, created.CreatedAt)
	assert.Equal(t, created.CreatedAt, created.UpdatedAt)

	highlightBody := `{"name":"Highlights","kind":"highlight_set","payload":{"v":1,"terms":[{"text":"urgent","color":"#ff0000"}]}}`
	resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/saved-queries",
		map[string]string{"Authorization": "Bearer " + testAPIKey}, highlightBody)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	assert.Equal(t, `"1"`, resp.Header.Get("ETag"))

	resp, body = rawJSONRequest(t, ts.URL, http.MethodGet,
		"/api/v1/saved-queries?kind=query&limit=1&offset=0",
		map[string]string{"X-Api-Key": testAPIKey}, "")
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.SavedQueryPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Equal(t, 1, page.Total)
	assert.Equal(t, 1, page.Limit)
	assert.Zero(t, page.Offset)
	require.Len(t, page.Items, 1)
	assert.Equal(t, created.ID, page.Items[0].ID)
	resp, body = rawJSONRequest(t, ts.URL, http.MethodGet,
		"/api/v1/saved-queries?limit=1&offset=1",
		map[string]string{"X-Api-Key": testAPIKey}, "")
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Equal(t, 2, page.Total)
	assert.Equal(t, 1, page.Offset)
	require.Len(t, page.Items, 1)
	assert.Equal(t, created.ID, page.Items[0].ID)

	resp, body = rawJSONRequest(t, ts.URL, http.MethodGet,
		"/api/v1/saved-queries/"+created.ID,
		map[string]string{"X-Api-Key": testAPIKey}, "")
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, `"1"`, resp.Header.Get("ETag"))
	var fetched api.SavedQuery
	require.NoError(t, json.Unmarshal([]byte(body), &fetched))
	assert.Equal(t, created, fetched)

	updatedPayload := `{"text":"tag:urgent OR tag:review","syntax":"advanced"}`
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPatch,
		"/api/v1/saved-queries/"+created.ID,
		map[string]string{"X-Api-Key": testAPIKey, "If-Match": etag},
		`{"description":"Updated synthetic definition","payload":`+updatedPayload+`}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, `"2"`, resp.Header.Get("ETag"))
	var updated api.SavedQuery
	require.NoError(t, json.Unmarshal([]byte(body), &updated))
	assert.EqualValues(t, 2, updated.Revision)
	assert.Equal(t, "Updated synthetic definition", updated.Description)
	assert.Equal(t, "tag:urgent OR tag:review", savedQueryText(t, updated.Payload))
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPatch,
		"/api/v1/saved-queries/"+created.ID,
		map[string]string{"X-Api-Key": testAPIKey, "If-Match": `"2"`},
		`{"description":"Updated synthetic definition","payload":`+updatedPayload+`}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, `"2"`, resp.Header.Get("ETag"))
	var noOp api.SavedQuery
	require.NoError(t, json.Unmarshal([]byte(body), &noOp))
	assert.Equal(t, updated, noOp)

	resp, body = rawJSONRequest(t, ts.URL, http.MethodPatch,
		"/api/v1/saved-queries/"+created.ID,
		map[string]string{"X-Api-Key": testAPIKey, "If-Match": `"1"`},
		`{"name":"Stale rename"}`)
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	assert.Equal(t, "stale_revision", decodeProblem(t, body).Code)

	resp, body = rawJSONRequest(t, ts.URL, http.MethodDelete,
		"/api/v1/saved-queries/"+created.ID,
		map[string]string{"X-Api-Key": testAPIKey, "If-Match": `"2"`}, "")
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, `"2"`, resp.Header.Get("ETag"))
	var deleted api.SavedQuery
	require.NoError(t, json.Unmarshal([]byte(body), &deleted))
	assert.Equal(t, updated, deleted)

	resp, body = rawJSONRequest(t, ts.URL, http.MethodGet,
		"/api/v1/saved-queries/"+created.ID,
		map[string]string{"X-Api-Key": testAPIKey}, "")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	assert.Equal(t, "not_found", decodeProblem(t, body).Code)
}

func savedQueryText(t *testing.T, payload []byte) string {
	t.Helper()
	var query struct {
		Text string `json:"text"`
	}
	require.NoError(t, json.Unmarshal(payload, &query))
	return query.Text
}

func TestSavedQueryHTTPNormalizesDuplicateFilterSetsAndAcceptsOptionalNulls(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	const duplicatePayload = `{"filters":{"paths":["/zeta","/alpha","/alpha"],"exclude_paths":["/archive","/archive"],"collection_ids":["11111111-1111-4111-8111-111111111111","11111111-1111-4111-8111-111111111111"],"exclude_collection_ids":["22222222-2222-4222-8222-222222222222","22222222-2222-4222-8222-222222222222"],"tag_ids":["33333333-3333-4333-8333-333333333333","33333333-3333-4333-8333-333333333333"],"exclude_tag_ids":["44444444-4444-4444-8444-444444444444","44444444-4444-4444-8444-444444444444"],"no_tags":null,"media_families":["document","document"],"mime_types":["application/pdf","application/pdf"],"extensions":["pdf","pdf"],"modified_after":null,"modified_before":null,"size_min":null,"size_max":null,"text_coverage":["complete","complete"],"has_duplicates":null,"collapse_duplicates":null}}`
	const deduplicatedPayload = `{"filters":{"paths":["/alpha","/zeta"],"exclude_paths":["/archive"],"collection_ids":["11111111-1111-4111-8111-111111111111"],"exclude_collection_ids":["22222222-2222-4222-8222-222222222222"],"tag_ids":["33333333-3333-4333-8333-333333333333"],"exclude_tag_ids":["44444444-4444-4444-8444-444444444444"],"media_families":["document"],"mime_types":["application/pdf"],"extensions":["pdf"],"text_coverage":["complete"]}}`

	withDuplicates, _ := createSavedQuery(t, ts.URL, "Duplicate filters", duplicatePayload)
	deduplicated, etag := createSavedQuery(t, ts.URL, "Unique filters", deduplicatedPayload)
	assert.Equal(t, deduplicated.Payload, withDuplicates.Payload)
	assert.Equal(t, deduplicated.Fingerprint, withDuplicates.Fingerprint)

	resp, body := rawJSONRequest(t, ts.URL, http.MethodPatch,
		"/api/v1/saved-queries/"+deduplicated.ID,
		map[string]string{"X-Api-Key": testAPIKey, "If-Match": etag},
		`{"payload":`+duplicatePayload+`}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var after api.SavedQuery
	require.NoError(t, json.Unmarshal([]byte(body), &after))
	assert.EqualValues(t, 1, after.Revision, "canonical duplicate-set no-op must keep its revision")
	assert.Equal(t, deduplicated.Payload, after.Payload)
	assert.Equal(t, deduplicated.Fingerprint, after.Fingerprint)
}

func TestSavedQueryHTTPNormalizesNamesBeforeApplyingUTF8ByteLimit(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	decomposed := strings.Repeat("U\u0308\u0304", 100)
	normalized := strings.Repeat("Ǖ", 100)
	require.Len(t, []rune(decomposed), 300)
	require.Len(t, []byte(normalized), 200)

	resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/saved-queries",
		map[string]string{"X-Api-Key": testAPIKey},
		fmt.Sprintf(`{"name":%q,"kind":"query","payload":{}}`, decomposed))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var created api.SavedQuery
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	assert.Equal(t, normalized, created.Name)

	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/saved-queries",
		map[string]string{"X-Api-Key": testAPIKey},
		fmt.Sprintf(`{"name":%q,"kind":"query","payload":{}}`, normalized))
	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
	assert.Equal(t, "exists", decodeProblem(t, body).Code)
	resp, body = rawJSONRequest(t, ts.URL, http.MethodDelete,
		"/api/v1/saved-queries/"+created.ID,
		map[string]string{"X-Api-Key": testAPIKey, "If-Match": `"1"`}, "")
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	patchTarget, patchETag := createSavedQuery(t, ts.URL, "Patch target", `{}`)
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPatch,
		"/api/v1/saved-queries/"+patchTarget.ID,
		map[string]string{"X-Api-Key": testAPIKey, "If-Match": patchETag},
		fmt.Sprintf(`{"name":%q}`, decomposed))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var patched api.SavedQuery
	require.NoError(t, json.Unmarshal([]byte(body), &patched))
	assert.Equal(t, normalized, patched.Name)
	assert.EqualValues(t, 2, patched.Revision)

	overLimit := strings.Repeat("é", 129)
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/saved-queries",
		map[string]string{"X-Api-Key": testAPIKey},
		fmt.Sprintf(`{"name":%q,"kind":"query","payload":{}}`, overLimit))
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Equal(t, "invalid_saved_query", decodeProblem(t, body).Code)
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPatch,
		"/api/v1/saved-queries/"+patchTarget.ID,
		map[string]string{"X-Api-Key": testAPIKey, "If-Match": `"2"`},
		fmt.Sprintf(`{"name":%q}`, overLimit))
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Equal(t, "invalid_saved_query", decodeProblem(t, body).Code)

	resp, body = rawJSONRequest(t, ts.URL, http.MethodGet,
		"/api/v1/saved-queries/"+patchTarget.ID,
		map[string]string{"X-Api-Key": testAPIKey}, "")
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var afterRejectedPatch api.SavedQuery
	require.NoError(t, json.Unmarshal([]byte(body), &afterRejectedPatch))
	assert.Equal(t, patched, afterRejectedPatch)
}

func TestSavedQueryHTTPDefaultsOmittedHighlightVersionOnCreateAndPatch(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	const omitted = `{"terms":[{"text":"omitted version","color":"#123abc"}]}`
	const explicit = `{"v":1,"terms":[{"text":"omitted version","color":"#123abc"}]}`

	resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/saved-queries",
		map[string]string{"X-Api-Key": testAPIKey},
		`{"name":"Omitted version","kind":"highlight_set","payload":`+omitted+`}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var omittedRecord api.SavedQuery
	require.NoError(t, json.Unmarshal([]byte(body), &omittedRecord))
	const canonical = `{"terms":[{"color":"#123abc","text":"omitted version"}],"v":1}`
	if got := string(omittedRecord.Payload); got != canonical {
		t.Fatalf("canonical omitted-version payload = %s, want %s", got, canonical)
	}

	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/saved-queries",
		map[string]string{"X-Api-Key": testAPIKey},
		`{"name":"Explicit version","kind":"highlight_set","payload":`+explicit+`}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var explicitRecord api.SavedQuery
	require.NoError(t, json.Unmarshal([]byte(body), &explicitRecord))
	assert.Equal(t, explicitRecord.Payload, omittedRecord.Payload)
	assert.Equal(t, explicitRecord.Fingerprint, omittedRecord.Fingerprint)

	resp, body = rawJSONRequest(t, ts.URL, http.MethodPatch,
		"/api/v1/saved-queries/"+explicitRecord.ID,
		map[string]string{"X-Api-Key": testAPIKey, "If-Match": `"1"`},
		`{"payload":`+omitted+`}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var patched api.SavedQuery
	require.NoError(t, json.Unmarshal([]byte(body), &patched))
	assert.EqualValues(t, 1, patched.Revision)
	assert.Equal(t, explicitRecord, patched)
}

func TestSavedQueryHTTPRejectsMalformedOrOversizeFilterSetsWithoutMutation(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	created, _ := createSavedQuery(t, ts.URL, "Stable filters", `{"filters":{"paths":["/records"]}}`)
	overLimit := strings.TrimSuffix(strings.Repeat(`"/records",`, 65), ",")

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"create object element", http.MethodPost, "/api/v1/saved-queries",
			`{"name":"Object element","kind":"query","payload":{"filters":{"paths":[{}]}}}`},
		{"create nested array element", http.MethodPost, "/api/v1/saved-queries",
			`{"name":"Array element","kind":"query","payload":{"filters":{"paths":[[]]}}}`},
		{"create duplicates over supplied cap", http.MethodPost, "/api/v1/saved-queries",
			`{"name":"Too many entries","kind":"query","payload":{"filters":{"paths":[` + overLimit + `]}}}`},
		{"patch object element", http.MethodPatch, "/api/v1/saved-queries/" + created.ID,
			`{"payload":{"filters":{"paths":[{}]}}}`},
		{"patch nested array element", http.MethodPatch, "/api/v1/saved-queries/" + created.ID,
			`{"payload":{"filters":{"paths":[[]]}}}`},
		{"patch duplicates over supplied cap", http.MethodPatch, "/api/v1/saved-queries/" + created.ID,
			`{"payload":{"filters":{"paths":[` + overLimit + `]}}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{"X-Api-Key": testAPIKey}
			if tc.method == http.MethodPatch {
				headers["If-Match"] = `"1"`
			}
			resp, body := rawJSONRequest(t, ts.URL, tc.method, tc.path, headers, tc.body)
			assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
			assert.Equal(t, "validation", decodeProblem(t, body).Code)

			resp, body = rawJSONRequest(t, ts.URL, http.MethodGet,
				"/api/v1/saved-queries/"+created.ID,
				map[string]string{"X-Api-Key": testAPIKey}, "")
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			var after api.SavedQuery
			require.NoError(t, json.Unmarshal([]byte(body), &after))
			assert.Equal(t, created, after)
		})
	}

	resp, body := rawJSONRequest(t, ts.URL, http.MethodGet,
		"/api/v1/saved-queries?limit=100&offset=0",
		map[string]string{"X-Api-Key": testAPIKey}, "")
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.SavedQueryPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Equal(t, 1, page.Total)
	require.Len(t, page.Items, 1)
	assert.Equal(t, created, page.Items[0])
}

func TestSavedQueryHTTPRejectsInvalidRequestsWithoutMutation(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	created, _ := createSavedQuery(t, ts.URL, "Collision", `{}`)

	tests := []struct {
		name       string
		method     string
		path       string
		headers    map[string]string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"duplicate name", http.MethodPost, "/api/v1/saved-queries", nil,
			`{"name":"Collision","kind":"query","payload":{}}`, http.StatusConflict, "exists"},
		{"nested duplicate payload key", http.MethodPost, "/api/v1/saved-queries", nil,
			`{"name":"Synthetic search","kind":"query","payload":{"text":"a","text":"b"}}`, http.StatusBadRequest, "validation"},
		{"malformed nested payload", http.MethodPost, "/api/v1/saved-queries", nil,
			`{"name":"Malformed","kind":"query","payload":{"filters":{"paths":"/wrong"}}}`, http.StatusUnprocessableEntity, "validation"},
		{"server field on create", http.MethodPost, "/api/v1/saved-queries", nil,
			`{"id":"11111111-1111-4111-8111-111111111111","name":"Owned","kind":"query","payload":{}}`, http.StatusUnprocessableEntity, "validation"},
		{"missing if match", http.MethodPatch, "/api/v1/saved-queries/" + created.ID, nil,
			`{"name":"Rename"}`, http.StatusPreconditionRequired, "precondition_required"},
		{"invalid if match", http.MethodPatch, "/api/v1/saved-queries/" + created.ID,
			map[string]string{"If-Match": `"bad"`}, `{"name":"Rename"}`, http.StatusBadRequest, "validation"},
		{"empty patch", http.MethodPatch, "/api/v1/saved-queries/" + created.ID,
			map[string]string{"If-Match": `"1"`}, `{}`, http.StatusUnprocessableEntity, "invalid_saved_query"},
		{"null patch field", http.MethodPatch, "/api/v1/saved-queries/" + created.ID,
			map[string]string{"If-Match": `"1"`}, `{"description":null}`, http.StatusUnprocessableEntity, "validation"},
		{"immutable kind", http.MethodPatch, "/api/v1/saved-queries/" + created.ID,
			map[string]string{"If-Match": `"1"`}, `{"kind":"highlight_set"}`, http.StatusUnprocessableEntity, "validation"},
		{"server revision on patch", http.MethodPatch, "/api/v1/saved-queries/" + created.ID,
			map[string]string{"If-Match": `"1"`}, `{"revision":2}`, http.StatusUnprocessableEntity, "validation"},
		{"null payload on patch", http.MethodPatch, "/api/v1/saved-queries/" + created.ID,
			map[string]string{"If-Match": `"1"`}, `{"payload":null}`, http.StatusUnprocessableEntity, "validation"},
		{"missing delete fence", http.MethodDelete, "/api/v1/saved-queries/" + created.ID, nil,
			"", http.StatusPreconditionRequired, "precondition_required"},
		{"unknown kind filter", http.MethodGet, "/api/v1/saved-queries?kind=other", nil,
			"", http.StatusUnprocessableEntity, "validation"},
		{"zero limit", http.MethodGet, "/api/v1/saved-queries?limit=0", nil,
			"", http.StatusUnprocessableEntity, "validation"},
		{"oversize limit", http.MethodGet, "/api/v1/saved-queries?limit=1001", nil,
			"", http.StatusUnprocessableEntity, "validation"},
		{"negative offset", http.MethodGet, "/api/v1/saved-queries?offset=-1", nil,
			"", http.StatusUnprocessableEntity, "validation"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{"X-Api-Key": testAPIKey}
			maps.Copy(headers, tc.headers)
			resp, body := rawJSONRequest(t, ts.URL, tc.method, tc.path, headers, tc.body)
			assert.Equal(t, tc.wantStatus, resp.StatusCode, body)
			assert.Equal(t, tc.wantCode, decodeProblem(t, body).Code)
		})
	}

	resp, body := rawJSONRequest(t, ts.URL, http.MethodGet,
		"/api/v1/saved-queries?limit=100&offset=0",
		map[string]string{"X-Api-Key": testAPIKey}, "")
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.SavedQueryPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Equal(t, 1, page.Total)
	require.Len(t, page.Items, 1)
	assert.Equal(t, created, page.Items[0])
}

func TestSavedQueryHTTPRequiresDaemonAuthentication(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	for _, headers := range []map[string]string{
		{"X-Api-Key": ""},
		{"X-Api-Key": "wrong"},
	} {
		resp, _ := rawJSONRequest(t, ts.URL, http.MethodGet,
			"/api/v1/saved-queries", headers, "")
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	}
}

func TestSavedQueryHTTPRejectsMutationsAfterAuditEnrollmentAndKeepsReads(t *testing.T) {
	ts, s := newTestServer(t, nil)
	created, _ := createSavedQuery(t, ts.URL, "Audit boundary", `{}`)
	c := client.New(ts.URL, testAPIKey)
	preview, err := c.PreviewAudit(t.Context(), client.AuditPreviewOptions{NodeID: s.RootID()})
	require.NoError(t, err)
	_, err = c.EnableAudit(t.Context(), preview.PreviewToken, true)
	require.NoError(t, err)

	mutations := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v1/saved-queries", `{"name":"After audit","kind":"query","payload":{}}`},
		{http.MethodPatch, "/api/v1/saved-queries/" + created.ID, `{"name":"Changed"}`},
		{http.MethodDelete, "/api/v1/saved-queries/" + created.ID, ""},
	}
	for _, tc := range mutations {
		headers := map[string]string{"X-Api-Key": testAPIKey}
		if tc.method != http.MethodPost {
			headers["If-Match"] = `"1"`
		}
		resp, body := rawJSONRequest(t, ts.URL, tc.method, tc.path, headers, tc.body)
		assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
		assert.Equal(t, "audit_mutation_unsupported", decodeProblem(t, body).Code)
	}

	resp, body := rawJSONRequest(t, ts.URL, http.MethodGet,
		"/api/v1/saved-queries/"+created.ID,
		map[string]string{"X-Api-Key": testAPIKey}, "")
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var after api.SavedQuery
	require.NoError(t, json.Unmarshal([]byte(body), &after))
	assert.Equal(t, created, after)
	resp, body = rawJSONRequest(t, ts.URL, http.MethodGet,
		"/api/v1/saved-queries?limit=100&offset=0",
		map[string]string{"X-Api-Key": testAPIKey}, "")
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.SavedQueryPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Equal(t, 1, page.Total)
}

func TestBrowserSessionAllowsOnlyExactSavedQueryRoutes(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &session))
	webHeaders := map[string]string{
		"X-Api-Key":          "",
		api.WebSessionHeader: session.Token,
	}

	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost,
		"/api/v1/saved-queries", webHeaders,
		`{"name":"Browser search","kind":"query","payload":{"text":"browser"}}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var saved api.SavedQuery
	require.NoError(t, json.Unmarshal([]byte(body), &saved))

	resp, body = rawJSONRequest(t, ts.URL, http.MethodGet,
		"/api/v1/saved-queries?kind=query&limit=100&offset=0", webHeaders, "")
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = rawJSONRequest(t, ts.URL, http.MethodGet,
		"/api/v1/saved-queries/"+saved.ID, webHeaders, "")
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	webHeaders["If-Match"] = `"1"`
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPatch,
		"/api/v1/saved-queries/"+saved.ID, webHeaders, `{"name":"Browser renamed"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	webHeaders["If-Match"] = `"2"`
	resp, body = rawJSONRequest(t, ts.URL, http.MethodDelete,
		"/api/v1/saved-queries/"+saved.ID, webHeaders, "")
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	for _, request := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPut, "/api/v1/saved-queries/" + saved.ID, `{}`},
		{http.MethodPost, "/api/v1/saved-queries/" + saved.ID, `{}`},
		{http.MethodPatch, "/api/v1/saved-queries/" + saved.ID + "?force=true", `{}`},
		{http.MethodGet, "/api/v1/saved-queries/" + saved.ID + "/run", ""},
		{http.MethodGet, "/api/v1/saved-query", ""},
	} {
		resp, _ = rawJSONRequest(t, ts.URL, request.method, request.path, webHeaders, request.body)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, request.method+" "+request.path)
	}
}
