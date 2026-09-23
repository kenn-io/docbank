package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestTUIPackageViewsReplaceNextPageAndRefreshFirst(t *testing.T) {
	for _, view := range []string{"packages", "members", "labels"} {
		t.Run(view, func(t *testing.T) {
			backend := newFakeBackend()
			model, err := New(t.Context(), backend)
			require.NoError(t, err)
			model.width, model.height, model.packagesOpen = 120, 24, true
			pkg := api.PackageSummary{PackageID: "11111111-1111-4111-8111-111111111111", PackageName: "synthetic", SnapshotID: "22222222-2222-4222-8222-222222222222"}
			model.packages = []api.PackageSummary{pkg}
			var first tea.Msg
			switch view {
			case "packages":
				first = packagesLoadedMsg{page: api.PackagePage{Items: []api.PackageSummary{{PackageName: "first-item"}}, NextAfter: "next-package"}}
				backend.packages = api.PackagePage{Items: []api.PackageSummary{{PackageName: "second-item"}}}
			case "members":
				model.packageMembersOpen, model.packageMembersPackage = true, pkg
				first = packageMembersLoadedMsg{packageID: pkg.PackageID, page: api.PackageMemberPage{
					Items: []api.PackageMember{{Ordinal: 250, DisplayName: "first-item"}}, NextAfterOrdinal: 250}}
				backend.packageMembers = map[string]api.PackageMemberPage{pkg.PackageID: {
					Items: []api.PackageMember{{Ordinal: 251, DisplayName: "second-item"}}}}
			case "labels":
				first = packageLabelLoadedMsg{label: "EXT000001", page: api.PackageLabelCandidatePage{
					Items: []store.PackageLabelRow{{Label: "EXT000001", LabelSet: "first-item"}}, NextCursor: "next-label"}}
				backend.packageLabels = api.PackageLabelCandidatePage{Items: []store.PackageLabelRow{{Label: "EXT000001", LabelSet: "second-item"}}}
			}
			model, _ = updateModel(t, model, first)
			model, cmd := updateModel(t, model, key('n'))
			require.NotNil(t, cmd, "the current view must offer its next page")
			model = runModelCommand(t, model, cmd)
			switch view {
			case "packages":
				assert.Equal(t, "next-package", backend.packagesAfter)
			case "members":
				assert.Equal(t, 250, backend.packageMembersAfter)
			case "labels":
				assert.Equal(t, "next-label", backend.packageLabelsCursor)
				assert.Equal(t, pkg.PackageID, backend.packageLabelsPackageID)
				assert.Equal(t, "EXT000001", backend.packageLabelsLabel)
			}
			assert.Contains(t, model.View().Content, "second-item")
			assert.NotContains(t, model.View().Content, "first-item")
			_, cmd = updateModel(t, model, key('n'))
			assert.Nil(t, cmd, "there is no next-page request after the last page")
			switch page := first.(type) {
			case packagesLoadedMsg:
				backend.packages = page.page
			case packageMembersLoadedMsg:
				backend.packageMembers[pkg.PackageID] = page.page
			case packageLabelLoadedMsg:
				backend.packageLabels = page.page
			}
			model, cmd = updateModel(t, model, key('r'))
			model = runModelCommand(t, model, cmd)
			assert.Empty(t, backend.packagesAfter)
			assert.Zero(t, backend.packageMembersAfter)
			assert.Empty(t, backend.packageLabelsCursor)
			assert.Contains(t, model.View().Content, "first-item")
			assert.NotContains(t, model.View().Content, "second-item")
			// A slower next-page response must not replace the refreshed first page.
			var stale tea.Msg
			switch view {
			case "packages":
				stale = packagesLoadedMsg{requestID: model.packagesRequestID - 1, page: api.PackagePage{Items: []api.PackageSummary{{PackageName: "stale-item"}}}}
			case "members":
				stale = packageMembersLoadedMsg{requestID: model.packagesRequestID - 1, packageID: pkg.PackageID,
					page: api.PackageMemberPage{Items: []api.PackageMember{{DisplayName: "stale-item"}}}}
			case "labels":
				stale = packageLabelLoadedMsg{requestID: model.packagesRequestID - 1, label: "EXT000001",
					page: api.PackageLabelCandidatePage{Items: []store.PackageLabelRow{{LabelSet: "stale-item"}}}}
			}
			model, _ = updateModel(t, model, stale)
			assert.Contains(t, model.View().Content, "first-item")
			assert.NotContains(t, model.View().Content, "stale-item")
		})
	}
}

func TestTUIPackageLabelsScrollWithoutChangingSelectedPackage(t *testing.T) {
	model, err := New(t.Context(), newFakeBackend())
	require.NoError(t, err)
	model.width, model.height, model.packagesOpen = 120, 20, true
	model.packages = []api.PackageSummary{{PackageName: "selected"}, {PackageName: "other"}}
	var matches []store.PackageLabelRow
	for i := range 30 {
		matches = append(matches, store.PackageLabelRow{Label: "EXT000001", LabelSet: fmt.Sprintf("set-%03d", i+1)})
	}
	model, _ = updateModel(t, model, packageLabelLoadedMsg{label: "EXT000001", page: api.PackageLabelCandidatePage{Items: matches, NextCursor: "more"}})
	assert.Contains(t, model.View().Content, "set-001")
	assert.NotContains(t, model.View().Content, "set-030")
	model, _ = updateModel(t, model, key(tea.KeyEnd))
	assert.Contains(t, model.View().Content, "set-030")
	assert.NotContains(t, model.View().Content, "set-001")
	assert.Contains(t, model.renderPackagesLocation(), "30+ match(es)")
	assert.Contains(t, model.renderPackagesFooter(), "30/30")
	assert.Contains(t, model.renderPackagesFooter(), "n next page")
	assert.Zero(t, model.packagesCursor)
	model, _ = updateModel(t, model, key(tea.KeyHome))
	assert.Contains(t, model.View().Content, "set-001")
}

func TestTUIPackageLabelReplyCannotReopenClosedLookup(t *testing.T) {
	backend := newFakeBackend()
	backend.packageLabels = api.PackageLabelCandidatePage{Items: []store.PackageLabelRow{{Label: "EXT000001"}}, NextCursor: "more"}
	model, err := New(t.Context(), backend)
	require.NoError(t, err)
	model.packagesOpen = true
	model.packages = []api.PackageSummary{{PackageID: "11111111-1111-4111-8111-111111111111"}}
	model, _ = updateModel(t, model, key('l'))
	model.searchInput.SetValue("EXT000001")
	model, pending := updateModel(t, model, key(tea.KeyEnter))
	require.NotNil(t, pending)
	model, _ = updateModel(t, model, key(tea.KeyEscape))
	model = runModelCommand(t, model, pending)
	assert.True(t, model.packagesOpen)
	assert.Empty(t, model.packageLabel)
	assert.Empty(t, model.packageLabelMatches)
	assert.Empty(t, model.packageLabelNextCursor)
	assert.False(t, model.packagesLoading)
}

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
	result := loading.loadPackageLabel("EXT000001", loading.packages[0].PackageID, "", loading.packagesRequestID)()
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

func TestTUIPackageViewsEscapeDisplayText(t *testing.T) {
	const hostile = "résumé\x1b[31m\x1b]8;;https://example.invalid\x07OPEN\x1b]8;;\x07\r\b\u009b"
	const escaped = `"résumé\x1b[31m\x1b]8;;https://example.invalid\aOPEN\x1b]8;;\a\r\b\u009b"`
	for _, field := range []string{"package name", "package heading", "member name", "label", "label set"} {
		t.Run(field, func(t *testing.T) {
			model, err := New(t.Context(), newFakeBackend())
			require.NoError(t, err)
			model.width, model.height, model.packagesOpen = 240, 24, true
			model.packages = []api.PackageSummary{{PackageName: "synthetic", Direction: "received", State: "complete"}}
			switch field {
			case "package name":
				model.packages[0].PackageName = hostile
			case "package heading":
				model.packageMembersOpen = true
				model.packageMembersPackage = api.PackageSummary{PackageName: hostile}
			case "member name":
				model.packageMembersOpen = true
				model.packageMembers = []api.PackageMember{{
					Ordinal: 1, DisplayName: hostile, DocumentKind: "file"}}
			case "label", "label set":
				model.packageLabel = "EXT000001"
				model.packageLabelMatches = []store.PackageLabelRow{{Label: "EXT000001", LabelSet: "received", Provenance: "received"}}
				if field == "label" {
					model.packageLabelMatches[0].Label = hostile
				} else {
					model.packageLabelMatches[0].LabelSet = hostile
				}
			}
			content := model.View().Content
			assert.Contains(t, content, escaped)
			for _, control := range []string{"\x1b[31m", "\x1b]8;", "\x07", "\r", "\b", "\u009b"} {
				assert.NotContains(t, content, control)
			}
		})
	}
}
