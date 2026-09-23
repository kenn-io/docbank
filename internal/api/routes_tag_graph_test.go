package api

import (
	"bytes"
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

	"go.kenn.io/docbank/internal/store"
)

func TestTagNeighborhoodRouteUsesExactFenceAndStableWireShape(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seed := createTagGraphAPIDocument(t, s, "seed.txt")
	peer := createTagGraphAPIDocument(t, s, "peer.txt")
	tag, err := s.CreateTag(t.Context(), "topical")
	require.NoError(t, err)
	seedChange, err := s.AssignTag(t.Context(), tag.ID, seed.ID, seed.Revision)
	require.NoError(t, err)
	peerChange, err := s.AssignTag(t.Context(), tag.ID, peer.ID, peer.Revision)
	require.NoError(t, err)

	mux := http.NewServeMux()
	humaAPI := humago.New(mux, huma.DefaultConfig("tag-graph-test", "0"))
	registerTagGraphRoutes(humaAPI, Deps{Store: s})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	request := TagNeighborhoodRequest{
		Fence: ResolvedDocumentSourceFence{VaultUID: s.VaultID(), ContentVersionIDs: []string{
			seedChange.Node.CurrentVersionID, peerChange.Node.CurrentVersionID,
		}},
		Seed:            TagGraphSeed{NodeID: seed.ID},
		AssignmentKinds: []string{store.TagAssignmentDocument},
		Limit:           20,
		MaxHops:         10,
		MaxVisited:      1000,
	}
	response, body := postTagGraph(t, server.URL, request)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	var result TagNeighborhoodResponse
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, s.VaultID(), result.VaultUID)
	assert.Equal(t, 2, result.DocumentCount)
	require.Len(t, result.Documents, 1)
	assert.Equal(t, peer.ID, result.Documents[0].NodeID)
	require.Len(t, result.Documents[0].SharedTags, 1)
	assert.Equal(t, tag.ID, result.Documents[0].SharedTags[0].ID)
	assert.Equal(t, store.TagAssignmentOriginLegacy, result.Documents[0].SharedTags[0].AssignmentOrigin)

	request.Limit = 101
	response, body = postTagGraph(t, server.URL, request)
	assert.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, string(body))
	assert.Contains(t, string(body), "expected number <= 100")
}

func createTagGraphAPIDocument(t *testing.T, s *store.Store, name string) store.Node {
	t.Helper()
	sum := sha256.Sum256([]byte(name))
	node, err := s.CreateFile(t.Context(), s.RootID(), name, hex.EncodeToString(sum[:]), 1, "text/plain")
	require.NoError(t, err)
	return node
}

func postTagGraph(t *testing.T, baseURL string, request TagNeighborhoodRequest) (*http.Response, []byte) {
	t.Helper()
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	response, err := http.Post(baseURL+"/api/v1/tags/neighborhood", "application/json", bytes.NewReader(encoded))
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response, body
}
