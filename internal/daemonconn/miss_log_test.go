package daemonconn_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestMissLogClientValidatesReturnedReceiptIDs(t *testing.T) {
	for _, id := range []string{"", strings.Repeat("a", 48), "invalid", strings.Repeat("z", 48)} {
		for _, documentSearch := range []bool{false, true} {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if documentSearch {
					assert.NoError(t, json.MarshalWrite(w, api.DocumentSearchReport{MissLogID: id, RequestedMode: "lexical", ActualMode: "lexical", Coverage: api.DocumentSearchCoverage{State: "complete"}, Results: []api.DocumentSearchResult{}}))
				} else {
					assert.NoError(t, json.MarshalWrite(w, api.SearchReport{MissLogID: id, Hits: []api.SearchHit{}, Limit: 50}))
				}
			}))
			c := daemonconn.New(server.URL, "key")
			var err error
			if documentSearch {
				_, err = c.SearchDocuments(t.Context(), api.DocumentSearchRequest{Query: "absent", Profile: "local", Mode: "lexical", Limit: 50, Fence: api.DocumentSourceFence{VaultUID: "11111111-1111-4111-8111-111111111111", ContentVersionIDs: []string{}}})
			} else {
				_, err = c.Search(t.Context(), "absent", 50)
			}
			server.Close()
			if id == "" || id == strings.Repeat("a", 48) {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		}
	}
}
