package api_test

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func importCollection(
	t *testing.T, tsURL string, client *http.Client, filename, content string, label *string,
) api.IngestReport {
	t.Helper()
	source := filepath.Join(t.TempDir(), filename)
	require.NoError(t, os.WriteFile(source, []byte(content), 0o600))
	body := map[string]any{"paths": []string{source}, "dest": "/inbox"}
	if label != nil {
		body["collection_label"] = *label
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, tsURL+"/api/v1/ingest", bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var report api.IngestReport
	require.NoError(t, json.UnmarshalRead(resp.Body, &report))
	require.NotEmpty(t, report.IngestID)
	return report
}

func TestCollectionsHTTPListsDetailsAndMembers(t *testing.T) {
	ts, s := newTestServer(t, nil)
	firstLabel := "First import"
	sourceDir := t.TempDir()
	alpha := filepath.Join(sourceDir, "alpha.txt")
	gamma := filepath.Join(sourceDir, "gamma.txt")
	require.NoError(t, os.WriteFile(alpha, []byte("alpha"), 0o600))
	require.NoError(t, os.WriteFile(gamma, []byte("gamma"), 0o600))
	resp, body := do(t, ts, http.MethodPost, "/api/v1/ingest", nil, map[string]any{
		"paths": []string{gamma, alpha}, "dest": "/inbox", "collection_label": firstLabel,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var first api.IngestReport
	require.NoError(t, json.Unmarshal([]byte(body), &first))
	require.NotEmpty(t, first.IngestID)
	second := importCollection(t, ts.URL, ts.Client(), "beta.txt", "longer beta", nil)

	embedded, err := s.BeginCallerSuppliedIngest(t.Context(), "agent", "synthetic assertion")
	require.NoError(t, err)
	_, _, err = s.IngestFile(t.Context(), embedded, s.RootID(), "embedded.txt",
		testHash("embedded"), int64(len("embedded")), "text/plain",
		"opaque://synthetic/embedded.txt", "")
	require.NoError(t, err)

	resp, body = get(t, ts, "/api/v1/collections?limit=1&offset=0", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var firstPage api.CollectionPage
	require.NoError(t, json.Unmarshal([]byte(body), &firstPage))
	assert.Equal(t, 2, firstPage.Total)
	assert.Equal(t, 1, firstPage.Limit)
	assert.Zero(t, firstPage.Offset)
	require.Len(t, firstPage.Items, 1)

	resp, body = get(t, ts, "/api/v1/collections?limit=1&offset=1", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var secondPage api.CollectionPage
	require.NoError(t, json.Unmarshal([]byte(body), &secondPage))
	assert.Equal(t, 2, secondPage.Total)
	assert.Equal(t, 1, secondPage.Limit)
	assert.Equal(t, 1, secondPage.Offset)
	require.Len(t, secondPage.Items, 1)
	assert.NotEqual(t, firstPage.Items[0].ID, secondPage.Items[0].ID)
	listed := map[string]api.Collection{
		firstPage.Items[0].ID: firstPage.Items[0], secondPage.Items[0].ID: secondPage.Items[0],
	}
	require.Contains(t, listed, first.IngestID)
	require.Contains(t, listed, second.IngestID)
	assert.Equal(t, "cli", listed[first.IngestID].SourceKind)
	assert.NotEmpty(t, listed[first.IngestID].SourceDescription)
	assert.Equal(t, int64(2), listed[first.IngestID].FileCount)
	assert.Equal(t, int64(len("alpha")+len("gamma")), listed[first.IngestID].TotalBytes)
	require.NotNil(t, listed[first.IngestID].Label)
	assert.Equal(t, firstLabel, *listed[first.IngestID].Label)
	assert.Equal(t, int64(1), listed[first.IngestID].LabelRevision)
	assert.NotEmpty(t, listed[first.IngestID].StartedAt)
	assert.NotEmpty(t, listed[first.IngestID].LabelUpdatedAt)

	resp, body = get(t, ts, "/api/v1/collections/"+first.IngestID, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var detail api.Collection
	require.NoError(t, json.Unmarshal([]byte(body), &detail))
	assert.Equal(t, listed[first.IngestID], detail)
	assert.Empty(t, resp.Header.Get("ETag"), "membership observations do not fence the label")

	resp, body = get(t, ts, "/api/v1/collections/"+first.IngestID+"/members?limit=1&offset=0", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var members api.CollectionMemberPage
	require.NoError(t, json.Unmarshal([]byte(body), &members))
	assert.Equal(t, detail, members.Collection)
	assert.Equal(t, 2, members.Total)
	assert.Equal(t, 1, members.Limit)
	assert.Zero(t, members.Offset)
	require.Len(t, members.Items, 1)
	assert.Equal(t, "/inbox/alpha.txt", members.Items[0].Path)
	assert.Equal(t, "alpha.txt", members.Items[0].Name)
	assert.Equal(t, int64(len("alpha")), members.Items[0].Size)

	resp, body = get(t, ts, "/api/v1/collections/"+first.IngestID+"/members?limit=1&offset=1", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var nextMembers api.CollectionMemberPage
	require.NoError(t, json.Unmarshal([]byte(body), &nextMembers))
	assert.Equal(t, detail, nextMembers.Collection)
	assert.Equal(t, 2, nextMembers.Total)
	require.Len(t, nextMembers.Items, 1)
	assert.Equal(t, "/inbox/gamma.txt", nextMembers.Items[0].Path)
}

func TestCollectionsHTTPValidatesAuthIdentityAndPageBounds(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	for name, key := range map[string]string{"missing": "", "wrong": "wrong-key"} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/collections", nil)
			require.NoError(t, err)
			req.Header["X-Api-Key"] = []string{key}
			resp, err := ts.Client().Do(req)
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		})
	}

	for _, path := range []string{
		"/api/v1/collections?limit=0", "/api/v1/collections?limit=1001",
		"/api/v1/collections?offset=-1", "/api/v1/collections/not-a-uuid",
		"/api/v1/collections/not-a-uuid/members", "/api/v1/collections/not-a-uuid/label",
	} {
		resp, body := get(t, ts, path, nil)
		if path == "/api/v1/collections?limit=0" || path == "/api/v1/collections?limit=1001" ||
			path == "/api/v1/collections?offset=-1" {
			assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
		} else {
			assert.Equal(t, http.StatusNotFound, resp.StatusCode, body)
		}
	}
}

func TestCollectionHTTPRetainsDirectLabelAccessWhenMembershipEmpties(t *testing.T) {
	ts, s := newTestServer(t, nil)
	label := "Reserved name"
	report := importCollection(t, ts.URL, ts.Client(), "retained.txt", "retained", &label)
	page, err := s.CollectionMembers(t.Context(), report.IngestID, 10, 0)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	_, _, err = s.Trash(t.Context(), page.Items[0].Node.ID, page.Items[0].Node.Revision)
	require.NoError(t, err)

	resp, body := get(t, ts, "/api/v1/collections", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var list api.CollectionPage
	require.NoError(t, json.Unmarshal([]byte(body), &list))
	assert.Zero(t, list.Total)
	assert.Empty(t, list.Items)

	resp, body = get(t, ts, "/api/v1/collections/"+report.IngestID, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var collection api.Collection
	require.NoError(t, json.Unmarshal([]byte(body), &collection))
	assert.Zero(t, collection.FileCount)
	assert.Zero(t, collection.TotalBytes)
	require.NotNil(t, collection.Label)
	assert.Equal(t, label, *collection.Label)

	resp, body = get(t, ts, "/api/v1/collections/"+report.IngestID+"/members", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var members api.CollectionMemberPage
	require.NoError(t, json.Unmarshal([]byte(body), &members))
	assert.Zero(t, members.Total)
	assert.Empty(t, members.Items)
	assert.Equal(t, 100, members.Limit)

	resp, body = get(t, ts, "/api/v1/collections/"+report.IngestID+"/label", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, `"1"`, resp.Header.Get("ETag"))
	var current api.CollectionLabel
	require.NoError(t, json.Unmarshal([]byte(body), &current))
	assert.NotContains(t, body, "file_count")
	assert.NotContains(t, body, "label_revision")
	assert.Equal(t, report.IngestID, current.IngestID)
	require.NotNil(t, current.Label)
	assert.Equal(t, label, *current.Label)
	assert.Equal(t, int64(1), current.Revision)
	assert.NotEmpty(t, current.UpdatedAt)
}

func TestCollectionLabelHTTPRevisionLifecycleAndValidation(t *testing.T) {
	ts, s := newTestServer(t, nil)
	report := importCollection(t, ts.URL, ts.Client(), "note.txt", "synthetic", nil)
	path := "/api/v1/collections/" + report.IngestID + "/label"

	resp, body := get(t, ts, path, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, `"1"`, resp.Header.Get("ETag"))
	var initial api.CollectionLabel
	require.NoError(t, json.Unmarshal([]byte(body), &initial))
	assert.Nil(t, initial.Label)
	assert.Equal(t, int64(1), initial.Revision)

	resp, body = do(t, ts, http.MethodPut, path, map[string]string{"If-Match": `"1"`},
		map[string]any{"label": nil})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, `"1"`, resp.Header.Get("ETag"))

	resp, body = do(t, ts, http.MethodPut, path, map[string]string{"If-Match": `"1"`},
		map[string]any{"label": "Review set"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, `"2"`, resp.Header.Get("ETag"))

	resp, body = do(t, ts, http.MethodPut, path, map[string]string{"If-Match": `"2"`},
		map[string]any{"label": "Review set"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, `"2"`, resp.Header.Get("ETag"))

	resp, body = do(t, ts, http.MethodPut, path, map[string]string{"If-Match": `"1"`},
		map[string]any{"label": "Different set"})
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	current, err := s.CollectionLabel(t.Context(), report.IngestID)
	require.NoError(t, err)
	require.NotNil(t, current.Label)
	assert.Equal(t, "Review set", *current.Label)

	resp, body = do(t, ts, http.MethodPut, path, map[string]string{"If-Match": `"2"`},
		map[string]any{"label": nil})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, `"3"`, resp.Header.Get("ETag"))
	var cleared api.CollectionLabel
	require.NoError(t, json.Unmarshal([]byte(body), &cleared))
	assert.Nil(t, cleared.Label)
	assert.Equal(t, int64(3), cleared.Revision)

	resp, body = do(t, ts, http.MethodPut, path, map[string]string{"If-Match": `"3"`},
		map[string]any{"label": nil})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, `"3"`, resp.Header.Get("ETag"))

	for name, headers := range map[string]map[string]string{
		"missing": nil,
		"invalid": {"If-Match": `"bad"`},
		"zero":    {"If-Match": "0"},
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := do(t, ts, http.MethodPut, path, headers, map[string]any{"label": "Next"})
			if name == "missing" {
				assert.Equal(t, http.StatusPreconditionRequired, resp.StatusCode, body)
			} else {
				assert.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
			}
		})
	}

	for name, request := range map[string]map[string]any{
		"missing label":    {},
		"unknown field":    {"label": "Next", "extra": true},
		"server revision":  {"label": "Next", "revision": 3},
		"server ingest id": {"label": "Next", "ingest_id": report.IngestID},
		"server timestamp": {"label": "Next", "updated_at": initial.UpdatedAt},
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := do(t, ts, http.MethodPut, path,
				map[string]string{"If-Match": `"3"`}, request)
			assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
		})
	}

	resp, body = do(t, ts, http.MethodPut, path, map[string]string{"If-Match": `"3"`},
		map[string]any{"label": "\t"})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"invalid_collection_label"`)
}

func TestCollectionLabelHTTPCollisionAndAuditRestriction(t *testing.T) {
	ts, s := newTestServer(t, nil)
	reserved := "Reserved"
	first := importCollection(t, ts.URL, ts.Client(), "first.txt", "first", &reserved)
	second := importCollection(t, ts.URL, ts.Client(), "second.txt", "second", nil)
	secondPath := "/api/v1/collections/" + second.IngestID + "/label"
	firstMembers, err := s.CollectionMembers(t.Context(), first.IngestID, 10, 0)
	require.NoError(t, err)
	require.Len(t, firstMembers.Items, 1)
	_, _, err = s.Trash(
		t.Context(), firstMembers.Items[0].Node.ID, firstMembers.Items[0].Node.Revision,
	)
	require.NoError(t, err)

	resp, body := do(t, ts, http.MethodPut, secondPath,
		map[string]string{"If-Match": `"1"`}, map[string]any{"label": reserved})
	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"exists"`)

	plan, err := s.PreviewInitialAudit(t.Context(), s.RootID(), "api", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)

	resp, body = do(t, ts, http.MethodPut, secondPath,
		map[string]string{"If-Match": `"1"`}, map[string]any{"label": "Blocked"})
	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"audit_mutation_unsupported"`)
}

func TestCollectionRoutesHaveExactBrowserSessionAllowList(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	report := importCollection(t, ts.URL, ts.Client(), "browser.txt", "browser", nil)
	resp, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var issued struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &issued))

	request := func(method, path string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, nil)
		require.NoError(t, err)
		req.Header["X-Api-Key"] = []string{""}
		req.Header.Set(api.WebSessionHeader, issued.Token)
		response, err := ts.Client().Do(req)
		require.NoError(t, err)
		return response
	}

	base := "/api/v1/collections/" + report.IngestID
	for _, path := range []string{
		"/api/v1/collections?limit=100&offset=0", base, base + "/members?limit=100&offset=0",
		base + "/label",
	} {
		resp := request(http.MethodGet, path)
		assert.Equal(t, http.StatusOK, resp.StatusCode, path)
		require.NoError(t, resp.Body.Close())
	}

	putBody := bytes.NewBufferString(`{"label":"Browser label"}`)
	req, err := http.NewRequest(http.MethodPut, ts.URL+base+"/label", putBody)
	require.NoError(t, err)
	req.Header["X-Api-Key"] = []string{""}
	req.Header.Set(api.WebSessionHeader, issued.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", `"1"`)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	for _, denied := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/collections"},
		{http.MethodDelete, base + "/label"},
		{http.MethodPatch, base + "/label"},
		{http.MethodPut, base + "/label?force=true"},
		{http.MethodPut, base + "/members"},
		{http.MethodGet, base + "/"},
		{http.MethodGet, base + "/label/extra"},
		{http.MethodGet, "/api/v1/collections/not/a/collection"},
	} {
		resp := request(denied.method, denied.path)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, "%s %s", denied.method, denied.path)
		require.NoError(t, resp.Body.Close())
	}
}
