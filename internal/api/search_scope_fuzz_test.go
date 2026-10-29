package api_test

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"mime"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
)

// Accepted requests must select exactly one population. Request decoding, Huma
// validation and processing validation all run, including malformed JSON seeds.
func FuzzDocumentSearchScope(f *testing.F) {
	ts, catalog := newTestServer(f, func(deps *api.Deps) {
		provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
		require.NoError(f, err)
		gate := api.NewOperationGate()
		deps.Gate = gate
		service, err := processing.NewService(processing.ServiceConfig{Catalog: deps.Store, Blobs: deps.Blobs, Gate: gate, SpoolDirectory: f.TempDir(), Profiles: map[string]processing.ProfileConfig{"private": {Profile: processingTestProfile(provider.Descriptor()), RenditionProvider: provider}}})
		require.NoError(f, err)
		deps.Processing = service
	})
	node, err := catalog.CreateFile(f.Context(), catalog.RootID(), "synthetic.txt", testHash("synthetic-scope-fuzz"), 1, "text/plain")
	require.NoError(f, err)
	base := api.DocumentSearchRequest{Query: "synthetic", Mode: "lexical", Profile: "private", Fence: api.DocumentSearchFence{VaultUID: "11111111-1111-4111-8111-111111111111"}, Scope: &api.DocumentSearchScope{Kind: "all_live"}}
	add := func(r api.DocumentSearchRequest) { data, err := json.Marshal(r); require.NoError(f, err); f.Add(data) }
	add(base)
	empty := base
	empty.Mode = "semantic"
	empty.Scope = &api.DocumentSearchScope{Kind: "all_live", Filters: &api.DocumentSourceFenceFilters{MIMEType: "application/x-no-match"}}
	add(empty)
	for _, filters := range []api.DocumentSourceFenceFilters{
		{MIMEType: "TEXT/PLAIN"}, {MIMEType: "application/pdf"},
		{ModifiedSince: "2099-01-01T00:00:00Z"}, {ModifiedBefore: "2000-01-01T00:00:00Z"},
		{UnderNodeID: catalog.RootID()}, {UnderNodeID: node.ID},
		{TagID: "33333333-3333-4333-8333-333333333333"},
	} {
		selected := base
		selected.Scope = &api.DocumentSearchScope{Kind: "all_live", Filters: &filters}
		add(selected)
	}
	both := base
	both.Fence.ContentVersionIDs = []string{"22222222-2222-4222-8222-222222222222"}
	add(both)
	explicit := both
	explicit.Scope = nil
	add(explicit)
	media := base
	media.MediaSources = []api.DocumentMediaSourceSelector{{SourceID: "synthetic", SourceVersionID: "synthetic", ContentVersionID: "22222222-2222-4222-8222-222222222222"}}
	add(media)
	invalid := base
	invalid.Scope = &api.DocumentSearchScope{Kind: "unknown"}
	add(invalid)
	neither := base
	neither.Scope = nil
	add(neither)
	f.Add([]byte(`{"scope":null,"fence":{}}`))
	f.Add([]byte(`{"scope":{"kind":"all_live"},"media_sources":null}`))
	f.Add([]byte(`{`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64<<10 {
			t.Skip()
		}
		// Stable seed identities survive the fresh vault created by each fuzz worker.
		data = bytes.ReplaceAll(data, []byte("11111111-1111-4111-8111-111111111111"), []byte(catalog.VaultID()))
		data = bytes.ReplaceAll(data, []byte("22222222-2222-4222-8222-222222222222"), []byte(node.CurrentVersionID))
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/v1/search", bytes.NewReader(data))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		response, err := ts.Client().Do(req)
		require.NoError(t, err)
		defer func() { require.NoError(t, response.Body.Close()) }()
		body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		require.NoError(t, err)
		if response.StatusCode != http.StatusOK {
			return
		}
		var request api.DocumentSearchRequest
		require.NoError(t, json.Unmarshal(data, &request))
		require.NotEqual(t, request.Scope == nil, len(request.Fence.ContentVersionIDs) == 0, "accepted an ambiguous population: %s", data)
		var report api.DocumentSearchReport
		require.NoError(t, json.Unmarshal(body, &report))
		require.Equal(t, catalog.VaultID(), request.Fence.VaultUID)
		if request.Scope != nil {
			require.Equal(t, "all_live", request.Scope.Kind)
			require.Nil(t, request.MediaSources)
			require.NotNil(t, report.Scope)
			require.Equal(t, "all_live", report.Scope.Kind)
			// The fixture has one known live file and no tags. This oracle
			// independently pins exact filters and population counts, even
			// when the query itself matches nothing.
			selected := true
			if filters := request.Scope.Filters; filters != nil {
				require.Empty(t, filters.TagID, "an unknown tag was accepted")
				require.True(t, filters.UnderNodeID == 0 || filters.UnderNodeID == catalog.RootID(), "an invalid directory was accepted")
				if filters.MIMEType != "" {
					mediaType, params, err := mime.ParseMediaType(filters.MIMEType)
					require.NoError(t, err)
					require.Empty(t, params)
					require.NotContains(t, mediaType, "*")
					selected = selected && mediaType == "text/plain"
				}
				modified, err := time.Parse(time.RFC3339Nano, node.ModifiedAt)
				require.NoError(t, err)
				var since, before time.Time
				if filters.ModifiedSince != "" {
					since, err = time.Parse(time.RFC3339Nano, filters.ModifiedSince)
					require.NoError(t, err)
					selected = selected && !modified.Before(since)
				}
				if filters.ModifiedBefore != "" {
					before, err = time.Parse(time.RFC3339Nano, filters.ModifiedBefore)
					require.NoError(t, err)
					selected = selected && modified.Before(before)
				}
				if filters.ModifiedSince != "" && filters.ModifiedBefore != "" {
					require.True(t, since.Before(before))
				}
			}
			require.NotNil(t, report.ObservedScopeCount)
			if selected {
				require.Equal(t, 1, *report.ObservedScopeCount)
			} else {
				require.Zero(t, *report.ObservedScopeCount)
				require.Empty(t, report.Results, "a metadata filter was widened")
			}
		} else {
			require.Nil(t, report.Scope)
		}
		for _, hit := range report.Results {
			require.Equal(t, catalog.VaultID(), hit.VaultUID)
			require.Equal(t, node.ID, hit.NodeID)
			require.Equal(t, node.CurrentVersionID, hit.ContentVersionID)
			if request.Scope == nil {
				require.Contains(t, request.Fence.ContentVersionIDs, hit.ContentVersionID)
			}
		}
	})
}
