package api_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

type photoRouteFixture struct {
	ts                    *httptest.Server
	s                     *testStore
	first, second         store.PhotoOwner
	firstNode, secondNode store.Node
	firstAsset            store.PhotoAsset
	firstHeaders          map[string]string
	secondHeaders         map[string]string
}

func newPhotoRouteFixture(t *testing.T) photoRouteFixture {
	t.Helper()
	ts, s := newTestServer(t, nil)
	first, err := s.CreatePhotoOwner(t.Context(), "First")
	require.NoError(t, err)
	second, err := s.CreatePhotoOwner(t.Context(), "Second")
	require.NoError(t, err)
	firstNode, err := s.CreateFile(store.WithPhotoOwner(t.Context(), first.ID), s.RootID(), "first.jpg", testHash("matrix-first"), 11, "image/jpeg")
	require.NoError(t, err)
	secondNode, err := s.CreateFile(store.WithPhotoOwner(t.Context(), second.ID), s.RootID(), "second.jpg", testHash("matrix-second"), 12, "image/jpeg")
	require.NoError(t, err)
	firstAsset, err := s.PhotoAssetForNode(store.WithPhotoOwner(t.Context(), first.ID), firstNode.ID)
	require.NoError(t, err)
	return photoRouteFixture{
		ts: ts, s: s, first: first, second: second, firstNode: firstNode, secondNode: secondNode,
		firstAsset:    firstAsset,
		firstHeaders:  map[string]string{"X-Docbank-Owner": first.ID},
		secondHeaders: map[string]string{"X-Docbank-Owner": second.ID},
	}
}

func TestWebSessionPhotoOwner(t *testing.T) {
	f := newPhotoRouteFixture(t)
	response, body := do(t, f.ts, http.MethodPost, "/api/daemon/web-session", map[string]string{"X-Docbank-Owner": f.first.ID}, nil)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &session))
	headers := map[string]string{api.WebSessionHeader: session.Token, "X-Api-Key": ""}
	response, body = do(t, f.ts, http.MethodPost, "/api/v1/workspace/queries", headers, map[string]any{"query": map[string]any{}, "page_size": 50})
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.Contains(t, body, "first.jpg")
	assert.NotContains(t, body, "second.jpg")
	response, body = do(t, f.ts, http.MethodPost, "/api/v1/workspace/queries", map[string]string{api.WebSessionHeader: session.Token, "X-Api-Key": "", "X-Docbank-Owner": f.second.ID}, map[string]any{"query": map[string]any{}, "page_size": 50})
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.Contains(t, body, "first.jpg")
}

func TestWebSessionAuditedVaultWithoutPhotoOwner(t *testing.T) {
	ts, s := newTestServer(t, nil)
	token := issueWebSession(t, ts)
	createFileWithContent(t, ts, s, "/ordinary.txt", "ordinary")
	response, body := do(t, ts, http.MethodPost, "/api/v1/workspace/queries", map[string]string{"X-Api-Key": "", api.WebSessionHeader: token}, map[string]any{"query": map[string]any{}, "page_size": 50})
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.Contains(t, body, "ordinary.txt")
	hash, size, err := s.Blobs.Write(strings.NewReader("photo"))
	require.NoError(t, err)
	image, err := s.CreateFile(t.Context(), s.RootID(), "photo.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(store.WithTrustedPhotoVisibility(t.Context()), image.ID)
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodPost, "/api/v1/workspace/queries", map[string]string{"X-Api-Key": "", api.WebSessionHeader: token}, map[string]any{"query": map[string]any{}, "page_size": 50})
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.NotContains(t, body, asset.ID)
}

func TestPhotoOwnerGlobalOperationalSummaries(t *testing.T) {
	ts, s := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = filepath.Join(t.TempDir(), "backup") })
	createFileWithContent(t, ts, s, "/backup.txt", "backup")
	response, body := do(t, ts, http.MethodPost, "/api/v1/backup/init", nil, map[string]any{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/backup/snapshots", nil, map[string]any{"tag": "matrix", "jobs": 1})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	token := issueWebSession(t, ts)
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: token}
	response, body = get(t, ts, "/api/v1/storage", headers)
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = get(t, ts, "/api/v1/backup/snapshots", headers)
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
}

func TestWebSessionPhotoOwnerResources(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	first, second := issueWebSession(t, ts), issueWebSession(t, ts)
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: first}
	response, body := do(t, ts, http.MethodPost, "/api/v1/workspace/queries", headers, map[string]any{"query": map[string]any{}, "page_size": 50})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.WorkspaceQueryResponse
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	response, body = do(t, ts, http.MethodPost, "/api/v1/workspace/queries/"+page.SnapshotID+"/pages", map[string]string{"X-Api-Key": "", api.WebSessionHeader: second}, map[string]string{"cursor": "tampered"})
	assert.Equal(t, http.StatusGone, response.StatusCode, body)
}

func TestPhotoOwnerRoutes(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	c := daemonconn.New(ts.URL, testAPIKey)
	owner, err := c.CreatePhotoOwner(t.Context(), "CLI owner")
	require.NoError(t, err)
	owners, err := c.PhotoOwners(t.Context())
	require.NoError(t, err)
	assert.NotEmpty(t, owners)
	renamed, err := c.RenamePhotoOwner(t.Context(), owner.ID, owner.Revision, "Renamed")
	require.NoError(t, err)
	assert.Equal(t, "Renamed", renamed.Name)
	require.NoError(t, c.RemovePhotoOwner(t.Context(), owner.ID, renamed.Revision))
}

func TestPhotoVisibilityReadRoutes(t *testing.T) {
	f := newPhotoRouteFixture(t)
	response, body := get(t, f.ts, "/api/v1/nodes/"+strconv.FormatInt(f.firstNode.ID, 10), f.secondHeaders)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = get(t, f.ts, "/api/v1/nodes/"+strconv.FormatInt(f.firstNode.ID, 10)+"/content", f.secondHeaders)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
}

func TestPhotoVisibilityPopulations(t *testing.T) {
	f := newPhotoRouteFixture(t)
	response, body := get(t, f.ts, "/api/v1/documents", f.firstHeaders)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.Contains(t, body, "first.jpg")
	assert.NotContains(t, body, "second.jpg")
}

func TestPhotoVisibilityAggregateRoutes(t *testing.T) {
	f := newPhotoRouteFixture(t)
	response, body := get(t, f.ts, "/api/v1/documents", f.secondHeaders)
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.NotContains(t, body, "first.jpg")
}

func TestPhotoVisibilityCachedResources(t *testing.T) {
	f := newPhotoRouteFixture(t)
	response, body := do(t, f.ts, http.MethodPost, "/api/v1/workspace/queries", f.firstHeaders, map[string]any{"query": map[string]any{}, "page_size": 50})
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
}

func TestPhotoVisibilityDerivedRoutes(t *testing.T) {
	f := newPhotoRouteFixture(t)
	response, body := get(t, f.ts, "/api/v1/nodes/"+strconv.FormatInt(f.firstNode.ID, 10)+"/content", f.secondHeaders)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
}

func TestPhotoVisibilityHistoryRoutes(t *testing.T) {
	f := newPhotoRouteFixture(t)
	response, body := get(t, f.ts, "/api/v1/audit/status?node_id="+strconv.FormatInt(f.firstNode.ID, 10), f.secondHeaders)
	assert.NotEqual(t, http.StatusInternalServerError, response.StatusCode, body)
}

func TestPhotoVisibilityMutationAtomicity(t *testing.T) {
	f := newPhotoRouteFixture(t)
	headers := map[string]string{"X-Docbank-Owner": f.second.ID, "If-Match": strconv.Quote("1")}
	response, body := do(t, f.ts, http.MethodPost, "/api/v1/photos/assets/"+f.firstAsset.ID+"/exclude", headers, map[string]any{"excluded": true})
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
}

func TestPhotoVisibilityDownloadTicket(t *testing.T) {
	f := newPhotoRouteFixture(t)
	response, body := get(t, f.ts, "/api/v1/nodes/"+strconv.FormatInt(f.firstNode.ID, 10)+"/content", f.secondHeaders)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
}

func TestProcessingPhotoOwnerConsent(t *testing.T) {
	ts, s := newTestServer(t, configureProcessingTestService(t))
	node := createFileWithContent(t, ts, s, "/consent.txt", "consent")
	token := issueWebSession(t, ts)
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: token}
	response, body := do(t, ts, http.MethodPost, "/api/v1/processing/plans", headers, api.ProcessingPlanRequest{Selector: api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "private"}})
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
}

func TestPhotoVisibilityRouteCoverage(t *testing.T) {
	_, fixture := newTestServer(t, nil)
	for _, path := range []string{"/api/v1/photos/assets/{asset_id}", "/api/v1/nodes/{id}", "/api/v1/documents", "/api/v1/workspace/queries/{id}/pages"} {
		assert.Contains(t, fixture.Server.API().OpenAPI().Paths, path)
	}
}
