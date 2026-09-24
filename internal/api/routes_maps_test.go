package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	mapviews "go.kenn.io/docbank/internal/maps"
	"go.kenn.io/docbank/internal/store"
)

func TestContentMapRoutesPreviewRevisionAndFrozenRead(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "synthetic.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	sum := sha256.Sum256([]byte("synthetic source"))
	node, err := s.CreateFile(t.Context(), s.RootID(), "source.txt", hex.EncodeToString(sum[:]), 16, "text/plain")
	require.NoError(t, err)
	identity, err := s.EnsureDocumentIdentity(t.Context(), node.ID)
	require.NoError(t, err)
	mux := http.NewServeMux()
	registerMapRoutes(humago.New(mux, huma.DefaultConfig("map-test", "0")), Deps{Store: s}, NewOperationGate())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	definition := store.ContentMapDefinition{Title: "Synthetic topic", Scope: "local", Sections: []store.ContentMapSection{{
		ID: "reading", Heading: "Read first", Ordering: "explicit", MaxEntries: 10,
		Include: []document.ContentMapPin{{DocumentUID: identity.DocumentUID, Mode: document.MapPinFollowCurrent}},
	}}}
	response, body := mapRequest(t, server.URL, http.MethodPost, "/api/v1/maps/plans", mapPlanRequest{Definition: definition}, "")
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	var plan store.ContentMapPlan
	require.NoError(t, json.Unmarshal(body, &plan))
	assert.NotEmpty(t, plan.DefinitionDigest)
	response, body = mapRequest(t, server.URL, http.MethodPost, "/api/v1/maps", mapWriteRequest{Definition: definition,
		DefinitionDigest: plan.DefinitionDigest}, "")
	require.Equal(t, http.StatusCreated, response.StatusCode, string(body))
	var created store.ContentMap
	require.NoError(t, json.Unmarshal(body, &created))
	assert.Equal(t, int64(1), created.Revision)
	assert.Equal(t, `"1"`, response.Header.Get("ETag"))

	response, body = mapRequest(t, server.URL, http.MethodPost, "/api/v1/maps/"+created.ID+"/snapshots", nil, `"1"`)
	require.Equal(t, http.StatusCreated, response.StatusCode, string(body))
	var frozen store.ContentMapSnapshot
	require.NoError(t, json.Unmarshal(body, &frozen))
	require.Len(t, frozen.Sections[0].Entries, 1)
	assert.Equal(t, node.CurrentVersionID, frozen.Sections[0].Entries[0].Member.ContentVersionID)
	response, body = mapRequest(t, server.URL, http.MethodPost, "/api/v1/maps/"+created.ID+"/refresh",
		mapRefreshRequest{PreviousSnapshotID: frozen.ID}, `"1"`)
	require.Equal(t, http.StatusCreated, response.StatusCode, string(body))
	var refreshed store.ContentMapRefresh
	require.NoError(t, json.Unmarshal(body, &refreshed))
	assert.Equal(t, frozen.ID, refreshed.Delta.BeforeSnapshotID)
	assert.NotEqual(t, frozen.ID, refreshed.Snapshot.ID)
	response, body = mapRequest(t, server.URL, http.MethodPost, "/api/v1/maps/"+created.ID+"/refresh",
		mapRefreshRequest{PreviousSnapshotID: frozen.ID}, `"1"`)
	assert.Equal(t, http.StatusPreconditionFailed, response.StatusCode, string(body))

	definition.Title = "Edited topic"
	response, body = mapRequest(t, server.URL, http.MethodPost, "/api/v1/maps/plans", mapPlanRequest{Definition: definition}, "")
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	require.NoError(t, json.Unmarshal(body, &plan))
	response, body = mapRequest(t, server.URL, http.MethodPatch, "/api/v1/maps/"+created.ID,
		mapWriteRequest{Definition: definition, DefinitionDigest: plan.DefinitionDigest}, `"1"`)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	response, body = mapRequest(t, server.URL, http.MethodPatch, "/api/v1/maps/"+created.ID,
		mapWriteRequest{Definition: definition, DefinitionDigest: plan.DefinitionDigest}, `"1"`)
	assert.Equal(t, http.StatusPreconditionFailed, response.StatusCode, string(body))
	response, body = mapRequest(t, server.URL, http.MethodGet, "/api/v1/map-snapshots/"+frozen.ID, nil, "")
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	var readBack store.ContentMapSnapshot
	require.NoError(t, json.Unmarshal(body, &readBack))
	assert.Equal(t, frozen, readBack)
}

func TestContentMapProposalRouteRequiresExplicitSave(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "synthetic.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	tag, err := s.CreateTag(t.Context(), "Research")
	require.NoError(t, err)
	mux := http.NewServeMux()
	registerMapRoutes(humago.New(mux, huma.DefaultConfig("map-test", "0")), Deps{Store: s}, NewOperationGate())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	response, body := mapRequest(t, server.URL, http.MethodPost, "/api/v1/maps/proposals",
		store.ContentMapProposalRequest{TagID: tag.ID}, "")
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	var plan store.ContentMapPlan
	require.NoError(t, json.Unmarshal(body, &plan))
	assert.Equal(t, []string{tag.ID}, plan.Definition.Sections[0].Selector.Filters.TagIDs)
	response, body = mapRequest(t, server.URL, http.MethodGet, "/api/v1/maps/"+tag.ID, nil, "")
	assert.Equal(t, http.StatusNotFound, response.StatusCode, string(body))
}

func TestContentMapViewRoutesReadAuthorizedDefinitionSnapshotAndDelta(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "synthetic.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	sum := sha256.Sum256([]byte("source"))
	node, err := s.CreateFile(t.Context(), s.RootID(), "source.txt", hex.EncodeToString(sum[:]), 6, "text/plain")
	require.NoError(t, err)
	identity, err := s.EnsureDocumentIdentity(t.Context(), node.ID)
	require.NoError(t, err)
	access := store.MapAccess{Owner: "local", AllSources: true}
	definition := store.ContentMapDefinition{Title: "Synthetic <topic>", Scope: "local",
		Sections: []store.ContentMapSection{{ID: "core", Heading: "Core", Ordering: "explicit", MaxEntries: 10,
			Include: []document.ContentMapPin{{DocumentUID: identity.DocumentUID, Mode: document.MapPinFollowCurrent}}}}}
	plan, err := s.PreviewContentMap(t.Context(), access, definition)
	require.NoError(t, err)
	created, err := s.CreateContentMap(t.Context(), access, definition, plan.DefinitionDigest)
	require.NoError(t, err)
	first, err := s.CreateContentMapSnapshot(t.Context(), access, created.ID, created.Revision)
	require.NoError(t, err)
	second, err := s.CreateContentMapSnapshot(t.Context(), access, created.ID, created.Revision)
	require.NoError(t, err)
	other, err := s.CreateContentMap(t.Context(), access, definition, plan.DefinitionDigest)
	require.NoError(t, err)
	foreign, err := s.CreateContentMapSnapshot(t.Context(), access, other.ID, other.Revision)
	require.NoError(t, err)
	mux := http.NewServeMux()
	registerMapRoutes(humago.New(mux, huma.DefaultConfig("map-test", "0")), Deps{Store: s}, NewOperationGate())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	for _, tc := range []struct {
		path string
		kind string
	}{
		{"/api/v1/map-views?kind=definition&map_id=" + created.ID, "definition"},
		{"/api/v1/map-views?kind=snapshot&snapshot_id=" + first.ID, "snapshot"},
		{"/api/v1/map-views?kind=delta&before_snapshot_id=" + first.ID + "&snapshot_id=" + second.ID, "delta"},
	} {
		response, body := mapRequest(t, server.URL, http.MethodGet, tc.path, nil, "")
		require.Equal(t, http.StatusOK, response.StatusCode, string(body))
		var view mapviews.ReadView
		require.NoError(t, json.Unmarshal(body, &view))
		assert.Equal(t, tc.kind, view.Kind)
		assert.Equal(t, created.ID, view.MapID)
		assert.NotEmpty(t, view.Markdown)
		assert.LessOrEqual(t, len(view.Markdown), mapviews.MaxMapReadBytes)
	}
	response, body := mapRequest(t, server.URL, http.MethodGet,
		"/api/v1/map-views?kind=delta&before_snapshot_id="+first.ID+"&snapshot_id="+foreign.ID, nil, "")
	assert.Equal(t, http.StatusNotFound, response.StatusCode, string(body))
	response, body = mapRequest(t, server.URL, http.MethodGet,
		"/api/v1/map-views?kind=snapshot&snapshot_id="+first.ID+"&limit=101", nil, "")
	assert.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, string(body))
	response, body = mapRequest(t, server.URL, http.MethodGet,
		"/api/v1/map-views?kind=snapshot&snapshot_id="+first.ID+"&limit=0", nil, "")
	assert.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, string(body))
}

func TestContentMapRouteErrorsDoNotLeakHiddenSources(t *testing.T) {
	for _, cause := range []error{store.ErrNotFound} {
		var problem *Error
		require.ErrorAs(t, mapRouteError(cause), &problem)
		assert.Equal(t, http.StatusNotFound, problem.Status)
		assert.NotContains(t, problem.Detail, "hidden")
	}
	var invalid *Error
	require.ErrorAs(t, mapRouteError(store.ErrInvalidContentMap), &invalid)
	assert.Equal(t, http.StatusUnprocessableEntity, invalid.Status)
	access, err := mapAccess(context.WithValue(t.Context(), authenticationContextKey{}, "browser"))
	require.Error(t, err)
	assert.False(t, access.AllSources)
}

func mapRequest(t *testing.T, baseURL, method, path string, input any, ifMatch string) (*http.Response, []byte) {
	t.Helper()
	var payload []byte
	if input != nil {
		var err error
		payload, err = json.Marshal(input)
		require.NoError(t, err)
	}
	request, err := http.NewRequest(method, baseURL+path, bytes.NewReader(payload))
	require.NoError(t, err)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		request.Header.Set("If-Match", ifMatch)
	}
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response, body
}
