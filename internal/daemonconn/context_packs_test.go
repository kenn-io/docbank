package daemonconn

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
)

func TestContextPackRejectsPassageOutsideRequestedFence(t *testing.T) {
	body := []byte("synthetic context")
	ref, err := document.NewPassageRefV1(document.PassageRefV1{
		VaultUID:         "11111111-1111-4111-8111-111111111111",
		DocumentUID:      "22222222-2222-4222-8222-222222222222",
		ContentVersionID: "44444444-4444-4444-8444-444444444444",
		SourceSHA256:     strings.Repeat("a", 64), RenditionBuildID: strings.Repeat("b", 64),
		AttachmentID: strings.Repeat("c", 64),
	}, body, 0, len(body))
	require.NoError(t, err)
	seed := ref
	seed.ContentVersionID = "33333333-3333-4333-8333-333333333333"
	fingerprint, err := processing.SourceFenceFingerprint(processing.SourceFence{
		VaultUID: ref.VaultUID, ContentVersionIDs: []string{seed.ContentVersionID}})
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/api/v1/context-packs", request.URL.Path)
		response.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(response, api.ContextPackResponse(processing.ContextPack{
			FenceFingerprint: fingerprint,
			Coverage:         processing.ContextCoverage{RequestedSources: 1, AvailableSources: 1, SelectedSources: 1},
			Passages: []processing.ContextPassage{{Ref: ref, Text: string(body), Path: "/synthetic.md",
				Reasons: []string{"exact_seed"}}}, Omitted: map[string]int{}, Complete: true,
		})))
	}))
	t.Cleanup(server.Close)
	_, err = New(server.URL, "synthetic-key").ContextPack(t.Context(), api.ContextPackRequest{
		Fence: api.DocumentSourceFence{VaultUID: ref.VaultUID,
			ContentVersionIDs: []string{"33333333-3333-4333-8333-333333333333"}},
		Seed: &seed, MaxBytes: 4096,
	})
	require.Error(t, err)
}
