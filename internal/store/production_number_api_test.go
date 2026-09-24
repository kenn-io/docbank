package store_test

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestPublishedProductionNumberHTTPExactAndRange(t *testing.T) {
	vault, root, job, _ := store.PublishedProductionPackageHTTPFixture(t)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	plan, err := vault.LoadProductionRenderPlan(t.Context(), job.ID)
	require.NoError(t, err)
	allocation, err := vault.ProductionNumberingForJob(t.Context(), job.ID)
	require.NoError(t, err)
	get := func(query, key string) (int, []byte) {
		t.Helper()
		path := "/api/v1/productions/numbers"
		if strings.HasPrefix(query, "query=") {
			path += "/candidates"
		}
		request, requestErr := http.NewRequestWithContext(t.Context(), http.MethodGet,
			httpServer.URL+path+"?"+query, nil)
		require.NoError(t, requestErr)
		if key != "" {
			request.Header.Set("X-Api-Key", key)
		}
		response, callErr := httpServer.Client().Do(request)
		require.NoError(t, callErr)
		body, readErr := io.ReadAll(response.Body)
		require.NoError(t, readErr)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, body
	}
	labelQuery := "label=" + url.QueryEscape(plan.Reservation.Numbers[0].Text)
	denied, _ := get(labelQuery, "")
	require.Equal(t, http.StatusUnauthorized, denied)
	status, body := get(labelQuery, cfg.Server.APIKey)
	require.Equal(t, http.StatusOK, status, string(body))
	var exact struct {
		Items []struct {
			Label           string `json:"label"`
			JobID           string `json:"job_id"`
			SourceVersionID string `json:"source_version_id"`
			ArtifactID      string `json:"artifact_id"`
			Volume          string `json:"volume"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(body, &exact))
	require.Len(t, exact.Items, 1)
	require.Equal(t, job.ID, exact.Items[0].JobID)
	require.Equal(t, plan.Reservation.Numbers[0].Text, exact.Items[0].Label)
	require.NotEmpty(t, exact.Items[0].SourceVersionID)
	require.NotEmpty(t, exact.Items[0].ArtifactID)
	require.NotEmpty(t, exact.Items[0].Volume)
	viaClient, err := daemonconn.New(httpServer.URL, cfg.Server.APIKey).ProductionNumbers(t.Context(),
		daemonconn.ProductionNumberQuery{Label: plan.Reservation.Numbers[0].Text})
	require.NoError(t, err)
	require.Len(t, viaClient.Items, 1)
	require.Equal(t, exact.Items[0].ArtifactID, viaClient.Items[0].ArtifactID)
	missing, _ := get("label=NOT-A-PUBLISHED-NUMBER", cfg.Server.APIKey)
	require.Equal(t, http.StatusNotFound, missing)

	rangeQuery := "namespace_id=" + allocation.NamespaceID + "&start_sequence=" +
		strconv.FormatInt(allocation.StartSequence, 10) + "&end_sequence=" +
		strconv.FormatInt(allocation.EndSequence, 10) + "&limit=1"
	status, body = get(rangeQuery, cfg.Server.APIKey)
	require.Equal(t, http.StatusOK, status, string(body))
	type numberPage struct {
		Items []struct {
			Label string `json:"label"`
		} `json:"items"`
		NextSequence int64 `json:"next_sequence"`
	}
	var page numberPage
	require.NoError(t, json.Unmarshal(body, &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, plan.Reservation.Numbers[0].Text, page.Items[0].Label)
	require.Equal(t, allocation.StartSequence, page.NextSequence)
	viaRangeClient, err := daemonconn.New(httpServer.URL, cfg.Server.APIKey).ProductionNumbers(t.Context(),
		daemonconn.ProductionNumberQuery{NamespaceID: allocation.NamespaceID,
			StartSequence: allocation.StartSequence, EndSequence: allocation.EndSequence, Limit: 1})
	require.NoError(t, err)
	require.Len(t, viaRangeClient.Items, 1)
	require.Equal(t, page.Items[0].Label, viaRangeClient.Items[0].Label)
	require.Equal(t, page.NextSequence, viaRangeClient.NextSequence)
	prefix := plan.Reservation.Numbers[0].Text[:len(plan.Reservation.Numbers[0].Text)-1]
	require.True(t, strings.HasPrefix(plan.Reservation.Numbers[1].Text, prefix))
	status, body = get("query="+url.QueryEscape(prefix)+"&limit=1", cfg.Server.APIKey)
	require.Equal(t, http.StatusOK, status, string(body))
	var candidates api.ProductionNumberCandidates
	require.NoError(t, json.Unmarshal(body, &candidates))
	require.Equal(t, "prefix", candidates.MatchKind)
	require.Len(t, candidates.Items, 1)
	require.True(t, candidates.Ambiguous)
	require.True(t, candidates.Truncated)
	require.Equal(t, plan.Reservation.Numbers[0].Text, candidates.Items[0].Label)
	viaCandidateClient, err := daemonconn.New(httpServer.URL, cfg.Server.APIKey).ProductionNumberCandidates(
		t.Context(), prefix, 1)
	require.NoError(t, err)
	require.Equal(t, candidates, viaCandidateClient)
	status, body = get("query="+url.QueryEscape(prefix[1:]), cfg.Server.APIKey)
	require.Equal(t, http.StatusOK, status, string(body))
	require.NoError(t, json.Unmarshal(body, &candidates))
	require.Equal(t, "substring", candidates.MatchKind)
	require.Len(t, candidates.Items, 2)
	status, body = get(rangeQuery+"&after_sequence="+strconv.FormatInt(page.NextSequence, 10), cfg.Server.APIKey)
	require.Equal(t, http.StatusOK, status, string(body))
	var continued numberPage
	require.NoError(t, json.Unmarshal(body, &continued))
	require.Len(t, continued.Items, 1)
	require.Equal(t, plan.Reservation.Numbers[1].Text, continued.Items[0].Label)
	require.Zero(t, continued.NextSequence)

	invalid, _ := get(labelQuery+"&"+rangeQuery, cfg.Server.APIKey)
	require.Equal(t, http.StatusUnprocessableEntity, invalid)
}
