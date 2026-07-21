package store

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	docsqlite "go.kenn.io/docbank/pkg/sqlite"
	"go.kenn.io/docbank/pkg/sqlite/modernc"
)

func TestBeginWalkRejectsOversizedPage(t *testing.T) {
	s := newTestStore(t)

	walker, err := s.BeginWalk(t.Context(), "/", 5001, false)
	if walker != nil {
		t.Cleanup(func() { require.NoError(t, walker.Close()) })
	}
	require.Nil(t, walker)
	require.ErrorContains(t, err, "walk page size must be between 1 and 5000")
}

func TestBeginWalkAcceptsMaximumPageSize(t *testing.T) {
	s := newTestStore(t)

	walker, err := s.BeginWalk(t.Context(), "/", MaxWalkPageSize, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, walker.Close()) })
	page, err := walker.Next(t.Context())
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, s.RootID(), page[0].Node.ID)
}

func TestWalkOrdersDuplicatePathsByNodeIDAndOptionallyIncludesTrash(t *testing.T) {
	s := newTestStore(t)
	first, err := s.CreateFile(
		t.Context(), s.RootID(), "same.txt", fakeHash("81"), 5, "text/plain",
	)
	require.NoError(t, err)
	trashed, _, err := s.Trash(t.Context(), first.ID, first.Revision)
	require.NoError(t, err)
	live, err := s.CreateFile(
		t.Context(), s.RootID(), "same.txt", fakeHash("82"), 4, "text/plain",
	)
	require.NoError(t, err)
	root, err := s.NodeByID(t.Context(), s.RootID())
	require.NoError(t, err)

	withTrash, err := s.BeginWalk(t.Context(), "/", 1, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, withTrash.Close()) })
	assert.Equal(t, []WalkEntry{
		{Path: "/", Node: root},
		{Path: "/same.txt", Node: trashed},
		{Path: "/same.txt", Node: live},
	}, collectStoreWalk(t, withTrash))

	liveOnly, err := s.BeginWalk(t.Context(), "/", 1, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, liveOnly.Close()) })
	assert.Equal(t, []WalkEntry{
		{Path: "/", Node: root},
		{Path: "/same.txt", Node: live},
	}, collectStoreWalk(t, liveOnly))
}

func TestWalkNonRootScopeNeverSeedsRequestedRootsSibling(t *testing.T) {
	for _, driver := range walkTestDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			s := newTestStoreWithDriver(t, driver.driver)
			scope, err := s.Mkdir(t.Context(), s.RootID(), "scope")
			require.NoError(t, err)
			child, err := s.Mkdir(t.Context(), scope.ID, "child")
			require.NoError(t, err)
			scope, err = s.NodeByID(t.Context(), scope.ID)
			require.NoError(t, err)
			_, err = s.Mkdir(t.Context(), s.RootID(), "zulu")
			require.NoError(t, err)

			walker, err := s.BeginWalk(t.Context(), "/scope", 1, false)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, walker.Close()) })
			assert.Equal(t, []WalkEntry{
				{Path: "/scope", Node: scope},
				{Path: "/scope/child", Node: child},
			}, collectStoreWalk(t, walker))
		})
	}
}

func TestWalkSetupAndPageWorkStayBoundedAcrossSubtreeSizes(t *testing.T) {
	for _, driver := range walkTestDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			var setupReads int64
			for _, size := range []int{8, 800} {
				s := newTestStoreWithDriver(t, driver.driver)
				for i := range size {
					_, err := s.Mkdir(t.Context(), s.RootID(), fmt.Sprintf("node-%04d", i))
					require.NoError(t, err)
				}

				walker, err := s.BeginWalk(t.Context(), "/", 7, false)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, walker.Close()) })
				before := walker.Stats()
				if setupReads == 0 {
					setupReads = before.SetupNodeReads
				}
				assert.Equal(t, setupReads, before.SetupNodeReads)
				assert.LessOrEqual(t, before.SetupNodeReads, int64(2))

				page, err := walker.Next(t.Context())
				require.NoError(t, err)
				require.Len(t, page, 7)
				after := walker.Stats()
				assert.Equal(t, int64(len(page)), after.LastPageRowsExamined)
				assert.LessOrEqual(t, after.LastPageIndexedSeeks, int64(2*len(page)))
			}
		})
	}
}

func TestWalkLiveOnlySeekUsesPartialIndexAcrossTrashedCardinality(t *testing.T) {
	for _, driver := range walkTestDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			for _, first := range []bool{true, false} {
				name := "next sibling"
				if first {
					name = "first child"
				}
				t.Run(name+" plan", func(t *testing.T) {
					s := newTestStoreWithDriver(t, driver.driver)
					query, args := walkSeekQuery(s.RootID(), false, "middle", 1, first)
					plan := explainQueryPlan(t, s, query, args...)
					assert.Contains(t, plan, "USING INDEX live_sibling_names")
				})
			}

			var baseline WalkStats
			for _, trashed := range []int{0, 4000} {
				s := newTestStoreWithDriver(t, driver.driver)
				insertTrashedWalkSiblings(t, s, trashed/2, "a")
				live, err := s.CreateFile(t.Context(), s.RootID(), "middle", fakeHash("91"), 1,
					"text/plain")
				require.NoError(t, err)
				insertTrashedWalkSiblings(t, s, trashed/2, "z")

				walker, err := s.BeginWalk(t.Context(), "/", 2, false)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, walker.Close()) })
				page, err := walker.Next(t.Context())
				require.NoError(t, err)
				require.Len(t, page, 2)
				assert.Equal(t, "/", page[0].Path)
				assert.Equal(t, WalkEntry{Path: "/middle", Node: live}, page[1])
				stats := walker.Stats()
				assert.Equal(t, int64(2), stats.LastPageRowsExamined)
				assert.Equal(t, int64(2), stats.LastPageIndexedSeeks)
				if trashed == 0 {
					baseline = stats
				} else {
					assert.Equal(t, baseline.LastPageRowsExamined, stats.LastPageRowsExamined)
					assert.Equal(t, baseline.LastPageIndexedSeeks, stats.LastPageIndexedSeeks)
				}
			}
		})
	}
}

func TestWalkIncludeTrashUsesBoundedDuplicateRangeSeeksAcrossCardinality(t *testing.T) {
	for _, driver := range walkTestDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			s := newTestStoreWithDriver(t, driver.driver)
			query, args := walkSeekQuery(s.RootID(), true, "duplicate", 1, false)
			plan := explainQueryPlan(t, s, query, args...)
			assert.Contains(t, plan, "parent_id=? AND name=? AND id>?")
			query, args = walkNextNameSeekQuery(s.RootID(), "duplicate")
			plan = explainQueryPlan(t, s, query, args...)
			assert.Contains(t, plan, "parent_id=? AND name>?")

			for _, count := range []int{4, 4000} {
				s := newTestStoreWithDriver(t, driver.driver)
				duplicateIDs := insertDuplicateTrashedWalkSiblings(t, s, count)
				zulu, err := s.Mkdir(t.Context(), s.RootID(), "zulu")
				require.NoError(t, err)
				walker, err := s.BeginWalk(t.Context(), "/", 2, true)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, walker.Close()) })

				wantIDs := append([]int64{s.RootID()}, duplicateIDs...)
				wantIDs = append(wantIDs, zulu.ID)
				pageCount := 3
				for pageIndex := range pageCount {
					page, err := walker.Next(t.Context())
					require.NoError(t, err)
					require.Len(t, page, 2)
					for offset, entry := range page {
						index := pageIndex*2 + offset
						assert.Equal(t, wantIDs[index], entry.Node.ID)
						wantPath := "/duplicate"
						if index == 0 {
							wantPath = "/"
						} else if index == len(wantIDs)-1 {
							wantPath = "/zulu"
						}
						assert.Equal(t, wantPath, entry.Path)
					}
					stats := walker.Stats()
					assert.Equal(t, int64(2), stats.LastPageRowsExamined)
					wantSeeks := int64(4)
					if pageIndex == 0 {
						wantSeeks = 3
					} else if count == 4 && pageIndex == 2 {
						wantSeeks = 6
					}
					assert.Equal(t, wantSeeks, stats.LastPageIndexedSeeks)
				}
			}
		})
	}
}

func explainQueryPlan(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	return strings.Join(details, "\n")
}

func insertTrashedWalkSiblings(t *testing.T, s *Store, count int, prefix string) {
	t.Helper()
	tx, err := s.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	for i := range count {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO nodes (
				parent_id, name, kind, created_at, modified_at, trashed_at
			) VALUES (?, ?, 'dir', ?, ?, ?)`,
			s.RootID(), fmt.Sprintf("%s-%04d", prefix, i),
			"2026-07-21T00:00:00.000000000Z", "2026-07-21T00:00:00.000000000Z",
			"2026-07-21T00:00:00.000000000Z")
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
}

func insertDuplicateTrashedWalkSiblings(t *testing.T, s *Store, count int) []int64 {
	t.Helper()
	tx, err := s.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	ids := make([]int64, 0, count)
	for range count {
		result, err := tx.ExecContext(t.Context(), `
			INSERT INTO nodes (
				parent_id, name, kind, created_at, modified_at, trashed_at
			) VALUES (?, 'duplicate', 'dir', ?, ?, ?)`,
			s.RootID(), "2026-07-21T00:00:00.000000000Z",
			"2026-07-21T00:00:00.000000000Z", "2026-07-21T00:00:00.000000000Z")
		require.NoError(t, err)
		id, err := result.LastInsertId()
		require.NoError(t, err)
		ids = append(ids, id)
	}
	require.NoError(t, tx.Commit())
	return ids
}

func walkTestDrivers() []struct {
	name   string
	driver docsqlite.Driver
} {
	return []struct {
		name   string
		driver docsqlite.Driver
	}{
		{name: "build default", driver: DefaultSQLiteDriver()},
		{name: "pure Go", driver: modernc.Driver{}},
	}
}

func TestWalkOrdersWideDuplicateDirectoryFrontierGlobally(t *testing.T) {
	s := newTestStore(t)
	wantIDs := []int64{s.RootID()}
	for i := range 20 {
		dir, err := s.Mkdir(t.Context(), s.RootID(), "duplicate")
		require.NoError(t, err)
		child, err := s.CreateFile(t.Context(), dir.ID, "child", fakeHash(fmt.Sprintf("%02d", i)), 1,
			"text/plain")
		require.NoError(t, err)
		wantIDs = append(wantIDs, dir.ID, child.ID)
		dir, err = s.NodeByID(t.Context(), dir.ID)
		require.NoError(t, err)
		_, _, err = s.Trash(t.Context(), dir.ID, dir.Revision)
		require.NoError(t, err)
	}
	sibling, err := s.Mkdir(t.Context(), s.RootID(), "duplicate-0")
	require.NoError(t, err)
	wantIDs = append(wantIDs, sibling.ID)

	walker, err := s.BeginWalk(t.Context(), "/", 3, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, walker.Close()) })
	entries := collectStoreWalk(t, walker)
	want := append([]WalkEntry(nil), entries...)
	sort.Slice(want, func(i, j int) bool {
		if want[i].Path != want[j].Path {
			return want[i].Path < want[j].Path
		}
		return want[i].Node.ID < want[j].Node.ID
	})
	assert.Equal(t, want, entries)
	assert.Len(t, entries, 42)
	gotIDs := make([]int64, 0, len(entries))
	for _, entry := range entries {
		gotIDs = append(gotIDs, entry.Node.ID)
	}
	assert.ElementsMatch(t, wantIDs, gotIDs)
}

func TestWalkEnforcesDepthAndPathBoundsIncrementally(t *testing.T) {
	t.Run("depth", func(t *testing.T) {
		for _, test := range []struct {
			name      string
			depth     int
			wantError bool
		}{
			{name: "at limit", depth: MaxWalkDepth},
			{name: "past limit", depth: MaxWalkDepth + 1, wantError: true},
		} {
			t.Run(test.name, func(t *testing.T) {
				s := newTestStore(t)
				parentID := s.RootID()
				for depth := 1; depth <= test.depth; depth++ {
					dir, err := s.Mkdir(t.Context(), parentID, "x")
					require.NoError(t, err)
					parentID = dir.ID
				}

				walker, err := s.BeginWalk(t.Context(), "/", MaxWalkPageSize, false)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, walker.Close()) })
				page, err := walker.Next(t.Context())
				if test.wantError {
					assert.Nil(t, page)
					require.ErrorContains(t, err, "walk depth exceeds 256")
					return
				}
				require.NoError(t, err)
				assert.Len(t, page, MaxWalkDepth+1)
			})
		}
	})

	t.Run("path bytes", func(t *testing.T) {
		s := newTestStore(t)
		_, err := s.Mkdir(t.Context(), s.RootID(), strings.Repeat("x", MaxWalkPathBytes))
		require.NoError(t, err)

		walker, err := s.BeginWalk(t.Context(), "/", 2, false)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, walker.Close()) })
		_, err = walker.Next(t.Context())
		require.ErrorContains(t, err, "walk path exceeds 16384 bytes")
	})
}

func TestWalkMidPageCancellationRollsBackFrontierAndStats(t *testing.T) {
	for _, driver := range walkTestDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			s := newTestStoreWithDriver(t, driver.driver)
			children := make([]Node, 0, 40)
			for i := range 40 {
				name := fmt.Sprintf("child-%02d", i)
				child, err := s.Mkdir(t.Context(), s.RootID(), name)
				require.NoError(t, err)
				children = append(children, child)
			}
			root, err := s.NodeByID(t.Context(), s.RootID())
			require.NoError(t, err)
			walker, err := s.BeginWalk(t.Context(), "/", 20, false)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, walker.Close()) })
			before := walker.Stats()

			cancelCtx, cancel := context.WithCancel(t.Context())
			walker.pageContextErr = func(ctx context.Context, rowsExamined int64) error {
				if rowsExamined == 1 {
					cancel()
				}
				return ctx.Err()
			}
			page, err := walker.Next(cancelCtx)
			assert.Nil(t, page)
			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, before, walker.Stats(),
				"a rolled-back page must not publish partial work counters")

			page, err = walker.Next(t.Context())
			require.NoError(t, err)
			require.Len(t, page, 20)
			assert.Equal(t, WalkEntry{Path: "/", Node: root}, page[0])
			for i := 1; i < 20; i++ {
				assert.Equal(t, WalkEntry{
					Path: fmt.Sprintf("/child-%02d", i-1), Node: children[i-1],
				}, page[i])
			}
		})
	}
}

func newTestStoreWithDriver(t *testing.T, driver docsqlite.Driver) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "docbank.db"), driver)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func collectStoreWalk(t *testing.T, walker *Walker) []WalkEntry {
	t.Helper()
	var entries []WalkEntry
	for {
		page, err := walker.Next(t.Context())
		if err != nil {
			require.ErrorIs(t, err, io.EOF)
			return entries
		}
		entries = append(entries, page...)
	}
}
