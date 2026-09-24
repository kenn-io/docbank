package daemonconn_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestTagNeighborhoodClientBindsRequestAndValidatesResponse(t *testing.T) {
	const (
		vaultID  = "11111111-1111-4111-8111-111111111111"
		firstID  = "22222222-2222-4222-8222-222222222222"
		secondID = "33333333-3333-4333-8333-333333333333"
		tagID    = "44444444-4444-4444-8444-444444444444"
	)
	request := api.TagNeighborhoodRequest{
		Fence: api.ResolvedDocumentSourceFence{VaultUID: vaultID, ContentVersionIDs: []string{firstID, secondID}},
		Seed:  api.TagGraphSeed{NodeID: 7}, AssignmentKinds: []string{store.TagAssignmentDocument},
		Limit: 20, MaxHops: 2, MaxVisited: 1000,
	}
	weight := store.TagRarityWeight(2, 2)
	valid := api.TagNeighborhoodResponse{
		VaultUID: vaultID, DocumentCount: 2, VisitedNodes: 3,
		Documents: []api.TagGraphDocument{{
			NodeID: 8, ContentVersionID: secondID, Name: "peer.txt", Path: "/peer.txt",
			ModifiedAt: "2026-09-22T00:00:00Z", Score: weight,
			SharedTags: []api.TagGraphTag{{
				ID: tagID, Name: "topic", ScopedDocumentCount: 2,
				Weight: weight, AssignmentOrigin: store.TagAssignmentOriginLegacy,
				Path: []api.TagGraphPathStep{},
			}},
			GraphPath: []api.TagGraphPathStep{
				{Kind: store.TagGraphNodeDocument, NodeID: 7},
				{Kind: store.TagGraphNodeTag, TagID: tagID},
				{Kind: store.TagGraphNodeDocument, NodeID: 8},
			},
		}},
		Tags: []api.TagGraphTag{{
			ID: tagID, Name: "topic", ScopedDocumentCount: 2,
			Weight: weight, AssignmentOrigin: store.TagAssignmentOriginLegacy,
			Path: []api.TagGraphPathStep{
				{Kind: store.TagGraphNodeDocument, NodeID: 7},
				{Kind: store.TagGraphNodeTag, TagID: tagID},
			},
		}},
	}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/tags/neighborhood", r.URL.Path)
		assert.Equal(t, "secret", r.Header.Get("X-Api-Key"))
		var received api.TagNeighborhoodRequest
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &received, json.RejectUnknownMembers(true))) {
			return
		}
		assert.Equal(t, request, received)
		assert.NoError(t, json.MarshalWrite(w, valid))
	}))
	t.Cleanup(server.Close)

	result, err := daemonconn.New(server.URL, "secret").TagNeighborhood(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, valid, result)
	assert.Equal(t, int64(1), calls.Load())

	request.Limit = 101
	_, err = daemonconn.New(server.URL, "secret").TagNeighborhood(t.Context(), request)
	require.ErrorContains(t, err, "request is invalid")
	assert.Equal(t, int64(1), calls.Load(), "invalid requests must fail before network access")
}

func TestTagNeighborhoodClientRejectsResponseOutsideFence(t *testing.T) {
	const (
		vaultID   = "11111111-1111-4111-8111-111111111111"
		firstID   = "22222222-2222-4222-8222-222222222222"
		outsideID = "33333333-3333-4333-8333-333333333333"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(t, json.MarshalWrite(w, api.TagNeighborhoodResponse{
			VaultUID: vaultID, DocumentCount: 1, VisitedNodes: 2,
			Documents: []api.TagGraphDocument{{NodeID: 9, ContentVersionID: outsideID, Score: 1}},
			Tags:      []api.TagGraphTag{},
		}))
	}))
	t.Cleanup(server.Close)
	_, err := daemonconn.New(server.URL, "secret").TagNeighborhood(t.Context(), api.TagNeighborhoodRequest{
		Fence: api.ResolvedDocumentSourceFence{VaultUID: vaultID, ContentVersionIDs: []string{firstID}},
		Seed:  api.TagGraphSeed{NodeID: 7}, AssignmentKinds: []string{store.TagAssignmentDocument},
		Limit: 20, MaxHops: 2, MaxVisited: 1000,
	})
	require.ErrorContains(t, err, "outside the requested source fence")
}

func TestTagNeighborhoodClientValidatesTagSeedPaths(t *testing.T) {
	const (
		vaultID  = "11111111-1111-4111-8111-111111111111"
		version  = "22222222-2222-4222-8222-222222222222"
		seedTag  = "33333333-3333-4333-8333-333333333333"
		otherTag = "44444444-4444-4444-8444-444444444444"
	)
	request := api.TagNeighborhoodRequest{
		Fence: api.ResolvedDocumentSourceFence{VaultUID: vaultID, ContentVersionIDs: []string{version}},
		Seed:  api.TagGraphSeed{TagID: seedTag}, AssignmentKinds: []string{store.TagAssignmentDocument},
		Limit: 20, MaxHops: 3, MaxVisited: 1000,
	}
	weight := store.TagRarityWeight(1, 1)
	valid := api.TagNeighborhoodResponse{
		VaultUID: vaultID, DocumentCount: 1, VisitedNodes: 3,
		Documents: []api.TagGraphDocument{{
			NodeID: 7, ContentVersionID: version, Name: "one.txt", Path: "/one.txt",
			ModifiedAt: "2026-09-22T00:00:00Z", Score: weight,
			SharedTags: []api.TagGraphTag{{
				ID: seedTag, Name: "seed", ScopedDocumentCount: 1, Weight: weight,
				AssignmentOrigin: store.TagAssignmentOriginLegacy, Path: []api.TagGraphPathStep{},
			}},
			GraphPath: []api.TagGraphPathStep{
				{Kind: store.TagGraphNodeTag, TagID: seedTag},
				{Kind: store.TagGraphNodeDocument, NodeID: 7},
			},
		}},
		Tags: []api.TagGraphTag{{
			ID: otherTag, Name: "other", ScopedDocumentCount: 1, Weight: weight,
			AssignmentOrigin: store.TagAssignmentOriginLegacy,
			Path: []api.TagGraphPathStep{
				{Kind: store.TagGraphNodeTag, TagID: seedTag},
				{Kind: store.TagGraphNodeDocument, NodeID: 7},
				{Kind: store.TagGraphNodeTag, TagID: otherTag},
			},
		}},
	}
	for name, mutate := range map[string]func(*api.TagNeighborhoodResponse){
		"valid": func(*api.TagNeighborhoodResponse) {},
		"wrong start": func(response *api.TagNeighborhoodResponse) {
			response.Tags[0].Path[0].TagID = otherTag
		},
		"wrong end": func(response *api.TagNeighborhoodResponse) {
			response.Tags[0].Path[2].TagID = seedTag
		},
	} {
		t.Run(name, func(t *testing.T) {
			response := valid
			response.Documents = append([]api.TagGraphDocument(nil), valid.Documents...)
			response.Tags = append([]api.TagGraphTag(nil), valid.Tags...)
			response.Tags[0].Path = append([]api.TagGraphPathStep(nil), valid.Tags[0].Path...)
			mutate(&response)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				assert.NoError(t, json.MarshalWrite(w, response))
			}))
			t.Cleanup(server.Close)
			_, err := daemonconn.New(server.URL, "secret").TagNeighborhood(t.Context(), request)
			if name == "valid" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "path")
			}
		})
	}
}
