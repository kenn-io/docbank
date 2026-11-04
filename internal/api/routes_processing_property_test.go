//go:build !darwin || arm64

package api_test

import (
	"testing"

	"hegel.dev/go/hegel"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestDocumentSearchEmptyFenceAutoResolvesActualModeProperty(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	client := daemonconn.New(ts.URL, testAPIKey)

	hegel.Test(t, func(ht *hegel.T) {
		limit := hegel.Draw(ht, hegel.Integers(1, 100))
		report, err := client.SearchDocuments(t.Context(), api.DocumentSearchRequest{
			Query: "synthetic empty fence", Mode: "auto", Profile: "private", Limit: limit,
			Fence: api.DocumentSearchFence{VaultUID: catalog.VaultID(), ContentVersionIDs: []string{}},
		})
		if err != nil {
			ht.Fatalf("empty-fence auto search rejected at limit %d: %v", limit, err)
		}
		if report.RequestedMode != "auto" || report.ActualMode != "lexical" || len(report.Results) != 0 {
			ht.Fatalf("unexpected empty-fence auto report at limit %d: %+v", limit, report)
		}
	}, hegel.WithDatabase(t.TempDir()))
}
