package retrieval

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestExpandedSearchRefreshesPathsAfterAncestorMove(t *testing.T) {
	catalog, err := store.Open(filepath.Join(t.TempDir(), "search.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	ancestor, err := catalog.Mkdir(t.Context(), catalog.RootID(), "before")
	require.NoError(t, err)
	file, err := catalog.CreateFile(t.Context(), ancestor.ID, "needle.txt", strings.Repeat("a", 64), 1, "text/plain")
	require.NoError(t, err)
	searcher, err := NewSearcher(SearcherConfig{Backend: catalog, Owner: "search-test", LeaseDuration: time.Minute})
	require.NoError(t, err)
	query := Query{Text: "needle", Mode: ModeLexical, Limit: 10}
	report, err := searcher.Search(t.Context(), query)
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	assert.Equal(t, "/before/needle.txt", report.Results[0].Path)

	ancestor, err = catalog.NodeByID(t.Context(), ancestor.ID)
	require.NoError(t, err)
	_, _, err = catalog.Move(t.Context(), ancestor.ID, catalog.RootID(), "after", ancestor.Revision)
	require.NoError(t, err)
	current, err := catalog.NodeByID(t.Context(), file.ID)
	require.NoError(t, err)
	require.Equal(t, file.Revision, current.Revision)

	revalidated, err := searcher.revalidateExpandedReport(t.Context(), query, report)
	require.NoError(t, err)
	require.Len(t, revalidated.Results, 1)
	assert.Equal(t, "/after/needle.txt", revalidated.Results[0].Path)
}
