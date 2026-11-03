package api_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestSearchValidityHTTP(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	ctx := t.Context()
	old := createFileWithContent(t, ts, catalog, "/old.txt", "synthetic ancient lease\n")
	next := createFileWithContent(t, ts, catalog, "/new.txt", "synthetic replacement lease\n")
	_, err := catalog.PutSupersession(ctx, next.ID, old.ID, old.Revision, new(""))
	require.NoError(t, err)
	_, err = catalog.PutEffectiveWindow(ctx, next.ID, next.Revision, "2025-01-01", "")
	require.NoError(t, err)
	c := daemonconn.New(ts.URL, testAPIKey)
	request := api.DocumentSearchRequest{Query: "txt", Limit: 2, Mode: "lexical", Profile: "private", AsOf: "2025-06-01", Fence: api.DocumentSourceFence{VaultUID: catalog.VaultID(), ContentVersionIDs: []string{old.CurrentVersionID, next.CurrentVersionID}}}
	report, err := c.SearchDocuments(ctx, request)
	require.NoError(t, err)
	require.Equal(t, "2025-06-01", report.AsOf)
	require.Len(t, report.Results, 2)
	for _, result := range report.Results {
		if result.NodeID == old.ID {
			require.NotNil(t, result.Validity)
			require.Equal(t, []api.DocumentValiditySuccessor{{NodeID: next.ID, Path: "/new.txt", EffectiveFrom: "2025-01-01"}}, result.Validity.SupersededBy)
			return
		}
	}
	t.Fatal("search results missing old document")
}
