package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func checkPeopleBrowseUnicode(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	for _, name := range []string{"Mary_Jane", "Jo O’Brien", "李·明", "李明", "a\u0301jane", "2Jane", "²Jane", "\u2160Jane", "A\u20ddJane", "Jane", "A [x].*", "A \\path", "𐐀Jane", "𐐀·Jane"} {
		_, err := s.CreatePerson(ctx, name, "operator")
		require.NoError(t, err)
	}
	for _, tc := range []struct {
		prefix string
		names  []string
	}{
		{"jane", []string{"Jane", "Mary_Jane", "𐐀·Jane"}},
		{"明", []string{"李·明"}},
		{"brien", []string{"Jo O’Brien"}},
		{"[x].*", []string{"A [x].*"}},
		{"\\path", []string{"A \\path"}},
		{"ane", []string{}},
	} {
		for _, recent := range []bool{false, true} {
			options := PeopleSearchOptions{RecentFirst: recent}
			var names []string
			afterName, afterID := "", ""
			for {
				page, _, err := s.PeopleByDisplayName(ctx, tc.prefix, afterName, afterID, 1, false, options)
				require.NoError(t, err)
				if len(page) == 0 {
					break
				}
				names = append(names, page[0].DisplayName)
				afterName, afterID, options.AfterRecent = page[0].DisplayNameFolded, page[0].PersonID, page[0].RecentRank
			}
			require.ElementsMatch(t, tc.names, names, "%q recent=%v", tc.prefix, recent)
		}
	}
}

func checkPeopleBrowseSnapshotPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	asset := albumAsset(t, s, "portrait.jpg")
	node := requireNodeByID(t, s, asset.Files[0].NodeID)
	_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID, "")
	require.NoError(t, err)
	options := PeopleSearchOptions{PhotoSummary: true}
	before, _, err := s.PeopleByDisplayName(ctx, "", "", "", 10, true, options)
	require.NoError(t, err)
	require.Len(t, before, 1)
	require.EqualValues(t, 1, before[0].PhotoSummary.LibraryCount)
	var writeErr error
	reader := postgresSnapshotReader(t, s, func() { _, writeErr = s.RetirePerson(ctx, person.PersonID, person.Revision) })
	during, _, err := reader.PeopleByDisplayName(ctx, "", "", "", 10, true, options)
	require.NoError(t, writeErr)
	require.NoError(t, err)
	require.Equal(t, before, during)
	after, _, err := s.PeopleByDisplayName(ctx, "", "", "", 10, true, options)
	require.NoError(t, err)
	require.Empty(t, after)
}

func checkPeopleBrowseNameIndexPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	// A populated catalog lets PostgreSQL choose a plan without disabling scans.
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		for i := range 2000 {
			if _, err := s.createPersonTx(ctx, tx, fmt.Sprintf("Person %04d", i), "operator"); err != nil {
				return err
			}
		}
		return nil
	}))
	// Planning must also work before autovacuum has collected statistics.
	rows, err := s.db.QueryContext(ctx, "EXPLAIN (COSTS OFF) "+s.peopleNamePageSQL("", 1), []byte(""), "", 24, 0, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var plan []string
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		plan = append(plan, line)
	}
	require.NoError(t, rows.Err())
	require.Contains(t, strings.Join(plan, "\n"), "Index Scan using persons_name_folded")
	page, _, err := s.PeopleByDisplayName(ctx, "", "", "", 24, false, PeopleSearchOptions{})
	require.NoError(t, err)
	require.Len(t, page, 24)
	require.Equal(t, "Person 0000", page[0].DisplayName)
	require.Equal(t, "Person 0023", page[23].DisplayName)
}
