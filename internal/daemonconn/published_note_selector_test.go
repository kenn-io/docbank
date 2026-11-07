package daemonconn_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestPublishedNoteSearchResponseAuthority(t *testing.T) {
	identity := api.PublishedNoteIdentity{VaultID: "11111111-1111-4111-8111-111111111111", Publication: "notebook", SourceRef: "folder/note.md", NodeID: 7, ContentVersionID: "22222222-2222-4222-8222-222222222222", Hash: strings.Repeat("a", 64), Size: 4, SourceRevision: 2, NamespaceRevision: 3}
	for _, kind := range []string{"valid", "missing", "publication", "version", "vault", "node", "source", "hash", "revision"} {
		t.Run(kind, func(t *testing.T) {
			id := identity
			switch kind {
			case "publication":
				id.Publication = "other"
			case "version":
				id.ContentVersionID = "33333333-3333-4333-8333-333333333333"
			case "vault":
				id.VaultID = "44444444-4444-4444-8444-444444444444"
			case "node":
				id.NodeID++
			case "source":
				id.SourceRef = "../note.md"
			case "hash":
				id.Hash = "bad"
			case "revision":
				id.NamespaceRevision = 0
			}
			ptr := &id
			if kind == "missing" {
				ptr = nil
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.MarshalWrite(w, api.DocumentSearchReport{RequestedMode: "lexical", ActualMode: "lexical", Coverage: api.DocumentSearchCoverage{State: "unknown"}, Degradations: []string{}, Results: []api.DocumentSearchResult{{PublishedNote: ptr, VaultUID: identity.VaultID, NodeID: 7, ContentVersionID: identity.ContentVersionID, Rank: 1, Score: 1, Path: "/note.md", LexicalRank: 1, Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}}}}})
			}))
			t.Cleanup(server.Close)
			report, err := daemonconn.New(server.URL, "synthetic-key").SearchDocuments(t.Context(), api.DocumentSearchRequest{Query: "note", Mode: "lexical", Profile: "local", Scope: &api.DocumentSearchScope{Kind: "all_live", Filters: &api.DocumentSourceFenceFilters{PublishedNotes: &api.PublishedNoteSelector{Publication: "notebook"}}}})
			if kind == "valid" {
				require.NoError(t, err)
				require.Len(t, report.Results, 1)
			} else {
				require.Error(t, err, "unbound authored identity must fail closed")
			}
		})
	}
}
