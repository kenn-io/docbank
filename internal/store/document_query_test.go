package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	docsqlite "go.kenn.io/docbank/sqlite"
	"go.kenn.io/docbank/sqlite/modernc"
)

func TestDocumentCatalogSortsCurrentLiveFilesDeterministically(t *testing.T) {
	drivers := []struct {
		name   string
		driver docsqlite.Driver
	}{{"default", DefaultSQLiteDriver()}, {"pure Go", modernc.Driver{}}}
	wants := map[DocumentCatalogSort][]string{
		DocumentCatalogSortPath:       {"/alpha/beta.md", "/alpha/zeta.pdf", "/beta/alpha.txt", "/gamma.bin"},
		DocumentCatalogSortName:       {"/beta/alpha.txt", "/alpha/beta.md", "/gamma.bin", "/alpha/zeta.pdf"},
		DocumentCatalogSortModifiedAt: {"/alpha/beta.md", "/gamma.bin", "/alpha/zeta.pdf", "/beta/alpha.txt"},
		DocumentCatalogSortSize:       {"/beta/alpha.txt", "/gamma.bin", "/alpha/beta.md", "/alpha/zeta.pdf"},
		DocumentCatalogSortMediaType:  {"/gamma.bin", "/alpha/zeta.pdf", "/alpha/beta.md", "/beta/alpha.txt"},
	}
	for _, driver := range drivers {
		for sortBy, ascending := range wants {
			for _, direction := range []DocumentCatalogDirection{
				DocumentCatalogDirectionAscending, DocumentCatalogDirectionDescending,
			} {
				t.Run(driver.name+"/"+string(sortBy)+"/"+string(direction), func(t *testing.T) {
					s := newTestStoreWithDriver(t, driver.driver)
					seedDocumentCatalog(t, s)
					page, err := s.ListDocuments(t.Context(), DocumentCatalogQuery{
						PathPrefix: "/", Sort: sortBy, Direction: direction, PageSize: 10,
					}, nil, DocumentCatalogTraversalNext)
					require.NoError(t, err)
					want := append([]string(nil), ascending...)
					if direction == DocumentCatalogDirectionDescending {
						reverseStrings(want)
					}
					assert.Equal(t, want, documentCatalogPaths(page.Items))
					assert.False(t, page.HasPrevious)
					assert.False(t, page.HasNext)
				})
			}
		}
	}
}

func TestDocumentCatalogNormalizesPathPrefixAndUsesStableTies(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.Mkdir(t.Context(), s.RootID(), "alpha")
	require.NoError(t, err)
	beta, err := s.Mkdir(t.Context(), s.RootID(), "beta")
	require.NoError(t, err)
	for index, parent := range []int64{alpha.ID, beta.ID} {
		node, createErr := s.CreateFile(t.Context(), parent, "shared.txt", fakeHash(fmt.Sprintf("d%d", index)), 7, "text/plain")
		require.NoError(t, createErr)
		_, updateErr := s.db.Exec(`UPDATE nodes SET modified_at=? WHERE id=?`,
			"2026-08-28T10:00:00.000000000Z", node.ID)
		require.NoError(t, updateErr)
	}

	page, err := s.ListDocuments(t.Context(), DocumentCatalogQuery{
		PathPrefix: "//alpha///", Sort: DocumentCatalogSortName,
		Direction: DocumentCatalogDirectionAscending, PageSize: 10,
	}, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	assert.Equal(t, "/alpha", page.Query.PathPrefix)
	assert.Equal(t, []string{"/alpha/shared.txt"}, documentCatalogPaths(page.Items))

	page, err = s.ListDocuments(t.Context(), DocumentCatalogQuery{
		PathPrefix: "/", Sort: DocumentCatalogSortSize,
		Direction: DocumentCatalogDirectionAscending, PageSize: 10,
	}, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	assert.Equal(t, []string{"/alpha/shared.txt", "/beta/shared.txt"}, documentCatalogPaths(page.Items),
		"equal primary values must break ties by canonical path then node ID")
}

func TestDocumentCatalogDefaultsAndHardPageMaximum(t *testing.T) {
	s := newTestStore(t)
	for i := range 251 {
		_, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("doc-%03d.txt", i),
			fakeHash(fmt.Sprintf("%04d", i)), int64(i+1), "text/plain")
		require.NoError(t, err)
	}

	page, err := s.ListDocuments(t.Context(), DocumentCatalogQuery{}, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	assert.Equal(t, 50, page.Query.PageSize)
	assert.Len(t, page.Items, 50)
	assert.True(t, page.HasNext)

	page, err = s.ListDocuments(t.Context(), DocumentCatalogQuery{PageSize: 250}, nil,
		DocumentCatalogTraversalNext)
	require.NoError(t, err)
	assert.Len(t, page.Items, 250)
	assert.True(t, page.HasNext)

	_, err = s.ListDocuments(t.Context(), DocumentCatalogQuery{PageSize: 251}, nil,
		DocumentCatalogTraversalNext)
	require.ErrorIs(t, err, ErrInvalidDocumentQuery)
}

func TestResolveDocumentSummariesPreservesOrderAndRejectsStaleIdentity(t *testing.T) {
	s := newTestStore(t)
	first, err := s.CreateFile(t.Context(), s.RootID(), "first.md", fakeHash("resolve-first"), 1, "text/markdown")
	require.NoError(t, err)
	second, err := s.CreateFile(t.Context(), s.RootID(), "second.md", fakeHash("resolve-second"), 2, "text/markdown")
	require.NoError(t, err)

	items, err := s.ResolveDocumentSummaries(t.Context(), []DocumentCatalogIdentity{
		{NodeID: second.ID, ContentVersionID: second.CurrentVersionID, Path: "/second.md"},
		{NodeID: first.ID, ContentVersionID: first.CurrentVersionID, Path: "/first.md"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"/second.md", "/first.md"}, documentCatalogPaths(items))

	_, err = s.ResolveDocumentSummaries(t.Context(), []DocumentCatalogIdentity{{
		NodeID: first.ID, ContentVersionID: first.CurrentVersionID, Path: "/moved.md",
	}})
	require.ErrorIs(t, err, ErrProcessingSourceFenceStaleVersion)
	_, err = s.ResolveDocumentSummaries(t.Context(), []DocumentCatalogIdentity{
		{NodeID: first.ID, ContentVersionID: first.CurrentVersionID, Path: "/first.md"},
		{NodeID: first.ID, ContentVersionID: first.CurrentVersionID, Path: "/first.md"},
	})
	require.ErrorIs(t, err, ErrProcessingSourceFenceStaleVersion)
}

func TestDocumentCatalogTraversesNextAndPreviousExactly(t *testing.T) {
	s := newTestStore(t)
	for i := range 5 {
		_, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("doc-%d.txt", i),
			fakeHash(fmt.Sprintf("p%d", i)), 1, "text/plain")
		require.NoError(t, err)
	}
	query := DocumentCatalogQuery{PageSize: 2}
	first, err := s.ListDocuments(t.Context(), query, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	second, err := s.ListDocuments(t.Context(), query, &first.LastPosition, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	third, err := s.ListDocuments(t.Context(), query, &second.LastPosition, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	back, err := s.ListDocuments(t.Context(), query, &third.FirstPosition, DocumentCatalogTraversalPrevious)
	require.NoError(t, err)
	start, err := s.ListDocuments(t.Context(), query, &back.FirstPosition, DocumentCatalogTraversalPrevious)
	require.NoError(t, err)

	assert.Equal(t, []string{"/doc-0.txt", "/doc-1.txt"}, documentCatalogPaths(first.Items))
	assert.Equal(t, []string{"/doc-2.txt", "/doc-3.txt"}, documentCatalogPaths(second.Items))
	assert.Equal(t, []string{"/doc-4.txt"}, documentCatalogPaths(third.Items))
	assert.Equal(t, documentCatalogPaths(second.Items), documentCatalogPaths(back.Items))
	assert.Equal(t, documentCatalogPaths(first.Items), documentCatalogPaths(start.Items))
	assert.False(t, first.HasPrevious)
	assert.True(t, first.HasNext)
	assert.True(t, second.HasPrevious)
	assert.True(t, second.HasNext)
	assert.True(t, third.HasPrevious)
	assert.False(t, third.HasNext)
}

func TestDocumentCatalogCursorPositionSurvivesConcurrentBoundaryDeletion(t *testing.T) {
	s := newTestStore(t)
	nodes := make([]Node, 4)
	for i := range nodes {
		var err error
		nodes[i], err = s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("doc-%d.txt", i),
			fakeHash(fmt.Sprintf("c%d", i)), 1, "text/plain")
		require.NoError(t, err)
	}
	query := DocumentCatalogQuery{PageSize: 2}
	first, err := s.ListDocuments(t.Context(), query, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), nodes[1].ID, nodes[1].Revision)
	require.NoError(t, err)
	second, err := s.ListDocuments(t.Context(), query, &first.LastPosition, DocumentCatalogTraversalNext)
	require.NoError(t, err)

	assert.Equal(t, []string{"/doc-2.txt", "/doc-3.txt"}, documentCatalogPaths(second.Items))
	for _, item := range second.Items {
		assert.NotContains(t, documentCatalogPaths(first.Items), item.Path)
	}
}

func TestDocumentCatalogPaginationMatrix(t *testing.T) {
	drivers := []struct {
		name   string
		driver docsqlite.Driver
	}{{"default", DefaultSQLiteDriver()}, {"pure Go", modernc.Driver{}}}
	sorts := []DocumentCatalogSort{
		DocumentCatalogSortPath, DocumentCatalogSortName, DocumentCatalogSortModifiedAt,
		DocumentCatalogSortSize, DocumentCatalogSortMediaType,
	}
	for _, driver := range drivers {
		for _, sortBy := range sorts {
			for _, direction := range []DocumentCatalogDirection{
				DocumentCatalogDirectionAscending, DocumentCatalogDirectionDescending,
			} {
				t.Run(driver.name+"/"+string(sortBy)+"/"+string(direction), func(t *testing.T) {
					s := newTestStoreWithDriver(t, driver.driver)
					nodes := seedTiedDocumentCatalog(t, s)
					want := []string{"/a/shared.txt", "/b/shared.txt", "/c/shared.txt",
						"/d/shared.txt", "/e/shared.txt", "/f/shared.txt"}
					if direction == DocumentCatalogDirectionDescending {
						reverseStrings(want)
					}
					query := DocumentCatalogQuery{Sort: sortBy, Direction: direction, PageSize: 2}
					first, err := s.ListDocuments(t.Context(), query, nil, DocumentCatalogTraversalNext)
					require.NoError(t, err)
					second, err := s.ListDocuments(t.Context(), query, &first.LastPosition,
						DocumentCatalogTraversalNext)
					require.NoError(t, err)
					third, err := s.ListDocuments(t.Context(), query, &second.LastPosition,
						DocumentCatalogTraversalNext)
					require.NoError(t, err)
					assert.Equal(t, want[:2], documentCatalogPaths(first.Items))
					assert.Equal(t, want[2:4], documentCatalogPaths(second.Items))
					assert.Equal(t, want[4:], documentCatalogPaths(third.Items))

					backSecond, err := s.ListDocuments(t.Context(), query, &third.FirstPosition,
						DocumentCatalogTraversalPrevious)
					require.NoError(t, err)
					backFirst, err := s.ListDocuments(t.Context(), query, &backSecond.FirstPosition,
						DocumentCatalogTraversalPrevious)
					require.NoError(t, err)
					assert.Equal(t, want[2:4], documentCatalogPaths(backSecond.Items))
					assert.Equal(t, want[:2], documentCatalogPaths(backFirst.Items))

					previousBoundary := third.FirstPosition
					deletedPrevious := nodes[want[4]]
					_, _, err = s.Trash(t.Context(), deletedPrevious.ID, deletedPrevious.Revision)
					require.NoError(t, err)
					afterPreviousDeletion, err := s.ListDocuments(t.Context(), query, &previousBoundary,
						DocumentCatalogTraversalPrevious)
					require.NoError(t, err)
					assert.Equal(t, want[2:4], documentCatalogPaths(afterPreviousDeletion.Items),
						"deleting a previous boundary must not restart or skip the prior live key")

					boundary := first.LastPosition
					deleted := nodes[want[1]]
					_, _, err = s.Trash(t.Context(), deleted.ID, deleted.Revision)
					require.NoError(t, err)
					afterDeletion, err := s.ListDocuments(t.Context(), query, &boundary,
						DocumentCatalogTraversalNext)
					require.NoError(t, err)
					assert.Equal(t, want[2:4], documentCatalogPaths(afterDeletion.Items),
						"deleting the exact boundary must not restart or skip the next live key")
				})
			}
		}
	}
}

func TestDocumentCatalogMaximumPathPositionTraversesBothDirections(t *testing.T) {
	s := newTestStore(t)
	_, err := s.CreateFile(t.Context(), s.RootID(), "a.txt", fakeHash("path-a"), 1, "text/plain")
	require.NoError(t, err)
	parentID := s.RootID()
	segments := make([]string, 0, MaxWalkDepth)
	for depth := range MaxWalkDepth - 1 {
		name := strings.Repeat("x", 63)
		if depth == 0 {
			name = "m" + name[1:]
		}
		dir, mkdirErr := s.Mkdir(t.Context(), parentID, name)
		require.NoError(t, mkdirErr)
		parentID = dir.ID
		segments = append(segments, name)
	}
	fileName := strings.Repeat("y", 63)
	_, err = s.CreateFile(t.Context(), parentID, fileName, fakeHash("max-path"), 1, "text/plain")
	require.NoError(t, err)
	segments = append(segments, fileName)
	maxPath := "/" + strings.Join(segments, "/")
	require.Len(t, maxPath, MaxWalkPathBytes)
	_, err = s.CreateFile(t.Context(), s.RootID(), "z.txt", fakeHash("path-z"), 1, "text/plain")
	require.NoError(t, err)

	query := DocumentCatalogQuery{PageSize: 1}
	first, err := s.ListDocuments(t.Context(), query, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	middle, err := s.ListDocuments(t.Context(), query, &first.LastPosition, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	last, err := s.ListDocuments(t.Context(), query, &middle.LastPosition, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	back, err := s.ListDocuments(t.Context(), query, &middle.FirstPosition,
		DocumentCatalogTraversalPrevious)
	require.NoError(t, err)
	assert.Equal(t, maxPath, middle.Items[0].Path)
	assert.Equal(t, "/z.txt", last.Items[0].Path)
	assert.Equal(t, "/a.txt", back.Items[0].Path)
}

func TestDocumentCatalogReturnsOnlySummaryProcessingAndActiveRenditionIdentity(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	request := renditionJobTestRequest(versions[0], profile)
	grantRenditionJobConsent(t, s, request)
	_, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{
		ID: waiter.AttachmentID, VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: build.ID, Profile: profile, AttachedAt: "2026-08-28T10:00:00.000000000Z",
	}
	require.NoError(t, s.AttachRenditionBuild(t.Context(), attachment))
	require.NoError(t, s.PublishRenditionHead(t.Context(), RenditionHeadRecord{
		ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: attachment.ID, PublishedAt: "2026-08-28T10:01:00.000000000Z",
	}))

	page, err := s.ListDocuments(t.Context(), DocumentCatalogQuery{
		PathPrefix: "/", PageSize: 10,
	}, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	item := page.Items[0]
	assert.Equal(t, versions[0], item.ContentVersionID)
	assert.Equal(t, "queued", item.LatestProcessingState)
	require.Len(t, item.ActiveRenditions, 1)
	assert.Equal(t, DocumentRenditionIdentity{
		ProfileFingerprint: profile.Fingerprint,
		AttachmentID:       attachment.ID,
		BuildID:            build.ID,
	}, item.ActiveRenditions[0])
}

func TestDocumentCatalogLatestProcessingStateUsesJobTransitionRecency(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	firstRequest := renditionJobTestRequest(versions[0], catalogProcessingProfile(t, false))
	grantRenditionJobConsent(t, s, firstRequest)
	first, _, err := s.EnqueueRenditionJob(t.Context(), firstRequest)
	require.NoError(t, err)

	secondProfile := catalogProcessingProfileWith(t, false, func(profile *document.ProcessingProfileV1) {
		profile.Rendition.MaxUnits++
	})
	secondRequest := renditionJobTestRequest(versions[0], secondProfile)
	grantRenditionJobConsent(t, s, secondRequest)
	second, _, err := s.EnqueueRenditionJob(t.Context(), secondRequest)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID)

	transitionedAt := time.Now().UTC().Add(time.Second)
	_, err = s.ClaimRenditionJob(t.Context(), first.ID, "worker:catalog", transitionedAt, time.Minute)
	require.NoError(t, err)

	page, err := s.ListDocuments(t.Context(), DocumentCatalogQuery{}, nil,
		DocumentCatalogTraversalNext)
	require.NoError(t, err)
	require.NotEmpty(t, page.Items)
	assert.Equal(t, "running", page.Items[0].LatestProcessingState)
}

func TestDocumentCatalogReturnsOnlyTheCurrentContentVersion(t *testing.T) {
	s := newTestStore(t)
	created, err := s.CreateFile(t.Context(), s.RootID(), "versioned.txt", fakeHash("old"), 3, "text/plain")
	require.NoError(t, err)
	current, _, err := s.ReplaceContent(t.Context(), created.ID, created.Revision,
		fakeHash("current"), 7, "text/markdown")
	require.NoError(t, err)

	page, err := s.ListDocuments(t.Context(), DocumentCatalogQuery{}, nil,
		DocumentCatalogTraversalNext)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, current.CurrentVersionID, page.Items[0].ContentVersionID)
	assert.Equal(t, int64(7), page.Items[0].Size)
	assert.Equal(t, "text/markdown", page.Items[0].MediaType)
}

func seedDocumentCatalog(t *testing.T, s *Store) {
	t.Helper()
	alpha, err := s.Mkdir(t.Context(), s.RootID(), "alpha")
	require.NoError(t, err)
	beta, err := s.Mkdir(t.Context(), s.RootID(), "beta")
	require.NoError(t, err)
	fixtures := []struct {
		parent   int64
		name     string
		size     int64
		media    string
		modified string
	}{
		{alpha.ID, "zeta.pdf", 40, "application/pdf", "2026-08-28T10:00:03.000000000Z"},
		{beta.ID, "alpha.txt", 10, "text/plain", "2026-08-28T10:00:04.000000000Z"},
		{alpha.ID, "beta.md", 30, "text/markdown", "2026-08-28T10:00:01.000000000Z"},
		{s.RootID(), "gamma.bin", 20, "application/octet-stream", "2026-08-28T10:00:02.000000000Z"},
	}
	for index, fixture := range fixtures {
		node, createErr := s.CreateFile(t.Context(), fixture.parent, fixture.name,
			fakeHash(fmt.Sprintf("catalog-%d", index)), fixture.size, fixture.media)
		require.NoError(t, createErr)
		_, updateErr := s.db.Exec(`UPDATE nodes SET modified_at=? WHERE id=?`, fixture.modified, node.ID)
		require.NoError(t, updateErr)
	}
	hidden, err := s.CreateFile(t.Context(), alpha.ID, "hidden.txt", fakeHash("hidden"), 5, "text/plain")
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), hidden.ID, hidden.Revision)
	require.NoError(t, err)
}

func seedTiedDocumentCatalog(t *testing.T, s *Store) map[string]Node {
	t.Helper()
	nodes := make(map[string]Node, 6)
	for index, directoryName := range []string{"a", "b", "c", "d", "e", "f"} {
		dir, err := s.Mkdir(t.Context(), s.RootID(), directoryName)
		require.NoError(t, err)
		node, err := s.CreateFile(t.Context(), dir.ID, "shared.txt",
			fakeHash(fmt.Sprintf("matrix-%d", index)), 7, "text/plain")
		require.NoError(t, err)
		_, err = s.db.Exec(`UPDATE nodes SET modified_at=? WHERE id=?`,
			"2026-08-28T10:00:00.000000000Z", node.ID)
		require.NoError(t, err)
		nodes["/"+directoryName+"/shared.txt"] = node
	}
	return nodes
}

func documentCatalogPaths(items []DocumentSummary) []string {
	paths := make([]string, len(items))
	for index, item := range items {
		paths[index] = item.Path
	}
	return paths
}

func reverseStrings(items []string) {
	for left, right := 0, len(items)-1; left < right; left, right = left+1, right-1 {
		items[left], items[right] = items[right], items[left]
	}
}
