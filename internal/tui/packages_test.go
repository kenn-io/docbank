package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestTUIPackagesLabelLookupStaysScopedToSelectedPackage(t *testing.T) {
	backend := newFakeBackend()
	backend.packageLabels = api.PackageLabelCandidatePage{Items: []store.PackageLabelRow{{
		PackageID: "11111111-1111-4111-8111-111111111111", Label: "EXT000001",
		LabelSet: "received", Provenance: "received", PageState: "unknown",
	}}}
	model, err := New(context.Background(), backend)
	require.NoError(t, err)
	model.width, model.height = 96, 28
	model.packagesOpen = true
	model.packages = []api.PackageSummary{{PackageID: "11111111-1111-4111-8111-111111111111", PackageName: "incoming"}}

	updated, _ := model.updatePackagesKeys(tea.KeyPressMsg{Code: 'l', Text: "l"})
	lookup, ok := updated.(Model)
	require.True(t, ok)
	require.True(t, lookup.packageLabelSearching)
	lookup.searchInput.SetValue("EXT000001")
	updated, _ = lookup.updatePackageLabelInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	loading, ok := updated.(Model)
	require.True(t, ok)
	assert.Equal(t, "/ ", loading.searchInput.Prompt)
	assert.Empty(t, loading.searchInput.Value())
	result := loading.loadPackageLabel("EXT000001", loading.packages[0].PackageID, loading.packagesRequestID)()
	updated, _ = loading.Update(result)
	resultModel, ok := updated.(Model)
	require.True(t, ok)
	view := resultModel.View().Content
	assert.Contains(t, view, "EXT000001")
	assert.Contains(t, view, "received")
}

func TestTUIPackagesScreenReportsTruncatedPagesAndHelp(t *testing.T) {
	model, err := New(t.Context(), newFakeBackend())
	require.NoError(t, err)
	model.width, model.height = 120, 32
	model.packagesOpen = true
	updated, _ := model.Update(packagesLoadedMsg{requestID: model.packagesRequestID,
		page: api.PackagePage{Items: []api.PackageSummary{{PackageName: "first"}}, NextAfter: "next"}})
	result, ok := updated.(Model)
	require.True(t, ok)
	assert.Contains(t, result.renderPackagesLocation(), "1+ package(s)")
	assert.Contains(t, strings.Join(result.helpLines(), "\n"), "Load-file package shortcuts")

	result.packagesOpen = false
	assert.Contains(t, result.renderFooter(), "K packages")
}

func TestPackageResponseCurrent(t *testing.T) {
	require.False(t, packageResponseCurrent(7, 6))
	require.True(t, packageResponseCurrent(7, 7))
}

func TestTUIPackagesScreenDropsStaleRequestIDs(t *testing.T) {
	model, err := New(t.Context(), newFakeBackend())
	require.NoError(t, err)
	model.packagesOpen, model.packagesRequestID = true, 7

	updated, _ := model.Update(packagesLoadedMsg{requestID: 6, page: api.PackagePage{Items: []api.PackageSummary{{PackageName: "stale"}}}})
	stale, ok := updated.(Model)
	require.True(t, ok)
	assert.Empty(t, stale.packages)

	updated, _ = model.Update(packagesLoadedMsg{requestID: 7, page: api.PackagePage{Items: []api.PackageSummary{{PackageName: "received-001"}}}})
	got, ok := updated.(Model)
	require.True(t, ok)
	require.Len(t, got.packages, 1)
	assert.Equal(t, "received-001", got.packages[0].PackageName)
}

func TestTUIPackagesScreenRendersReceivedAndProducedCounters(t *testing.T) {
	model, err := New(t.Context(), newFakeBackend())
	require.NoError(t, err)
	model.width, model.height = 96, 28
	model.packagesOpen = true
	model.packages = []api.PackageSummary{
		{PackageName: "incoming-001", Direction: "received", State: "complete", MemberCount: 2, PageCount: 3},
		{PackageName: "review-001", Direction: "produced", State: "complete", MemberCount: 2, PageCount: 2},
	}

	view := model.View().Content
	assert.Contains(t, view, "Load-file packages")
	assert.Contains(t, view, "incoming-001")
	assert.Contains(t, view, "received")
	assert.Contains(t, view, "complete · 2 documents · 3 pages")
	assert.Contains(t, view, "review-001")
	assert.Contains(t, view, "produced")
}
