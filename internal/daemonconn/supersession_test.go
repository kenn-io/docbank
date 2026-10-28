package daemonconn

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestSupersessionClientBindsMutationReceipts(t *testing.T) {
	for _, tc := range []struct {
		name             string
		nodeID, revision int64
		changed          bool
		etag             string
		bad              bool
	}{
		{"changed predecessor", 3, 8, true, `"8"`, false},
		{"unchanged predecessor", 3, 7, false, `"7"`, false},
		{"wrong node", 4, 8, true, `"8"`, true},
		{"wrong revision", 3, 7, true, `"8"`, true},
		{"wrong ETag", 3, 8, true, `"7"`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPut, r.Method)
				assert.Equal(t, "/api/v1/nodes/5/supersedes/3", r.URL.Path)
				assert.Equal(t, `"7"`, r.Header.Get("If-Match"))
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("ETag", tc.etag)
				_ = json.MarshalWrite(w, api.ValidityReceipt{Node: api.Node{ID: tc.nodeID, Revision: tc.revision}, Changed: tc.changed})
			}))
			t.Cleanup(server.Close)
			receipt, err := New(server.URL, "synthetic-key").PutSupersession(t.Context(), 5, 3, 7, "replacement")
			if tc.bad {
				require.Error(t, err)
				assert.True(t, IsResponseDecodeError(err))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.nodeID, receipt.Node.ID)
			assert.Equal(t, tc.changed, receipt.Changed)
		})
	}
}

func TestSupersessionClientBindsValidityDate(t *testing.T) {
	for _, tc := range []struct {
		name, date string
		nodeID     int64
		bad        bool
	}{
		{"requested", "2025-06-01", 3, false},
		{"wrong date", "2025-06-02", 3, true},
		{"wrong node", "2025-06-01", 4, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "2025-06-01", r.URL.Query().Get("as_of"))
				w.Header().Set("Content-Type", "application/json")
				_ = json.MarshalWrite(w, api.NodeValidity{Node: api.Node{ID: tc.nodeID, Revision: 1}, AsOf: tc.date, State: "current"})
			}))
			t.Cleanup(server.Close)
			view, err := New(server.URL, "synthetic-key").NodeValidity(t.Context(), 3, "2025-06-01")
			if tc.bad {
				require.Error(t, err)
				assert.True(t, IsResponseDecodeError(err))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "current", view.State)
		})
	}
}
