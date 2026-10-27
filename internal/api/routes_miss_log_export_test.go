package api_test

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/misslog"
	"go.kenn.io/docbank/internal/store"
)

func TestMissLogExportOwnerProjectionAndDisabledAccess(t *testing.T) {
	t.Parallel()
	ts, st := newTestServer(t, nil)
	receipt := misslog.Receipt{ID: strings.Repeat("a", 48), VaultUID: st.VaultID(), CreatedAt: time.Now().UTC(), Query: "=synthetic,private\nquestion", Feedback: &misslog.Feedback{Verdict: "none_in_vault", Note: "private note excluded"}, Hits: []misslog.Hit{{Rank: 1, NodeID: 7, Path: "/private-path.txt", EvidenceIDs: []string{"unrelated"}}}}
	raw, err := json.Marshal(receipt)
	require.NoError(t, err)
	require.NoError(t, st.AddMissLogRecord(t.Context(), store.MissLogRecord{ID: receipt.ID, CreatedAt: receipt.CreatedAt, Receipt: string(raw)}, time.Now(), 90, 5000))
	for _, format := range []string{"eval", "cards"} {
		response, body := get(t, ts, "/api/v1/miss-log/export?format="+format, nil)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
		var page misslog.ExportPage
		require.NoError(t, json.Unmarshal([]byte(body), &page))
		require.Len(t, page.Items, 1)
		require.Equal(t, receipt.Query, page.Items[0].Question)
		require.Empty(t, page.Items[0].Expected)
		require.True(t, page.Done)
		for _, excluded := range []string{"private note", "private-path", "unrelated", "capability", "triggers"} {
			require.NotContains(t, body, excluded)
		}
	}
	response, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &session))
	response, body = get(t, ts, "/api/v1/miss-log/export?format=eval", map[string]string{"X-Api-Key": "", api.WebSessionHeader: session.Token})
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	require.NotContains(t, body, "private")
	for _, query := range []string{"format=unknown", "format=eval&limit=101", "format=cards&offset=-1"} {
		response, body = get(t, ts, "/api/v1/miss-log/export?"+query, nil)
		require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	}
}
