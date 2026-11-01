package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReceiptCleanupIndexesSQLite(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, test := range []struct {
		name, query, index string
		argument           any
	}{
		{"purge", s.purgedPhotoReceiptsSQL(), "photo_change_receipts_xmp_file", "[2]"},
		{"prune", s.retainedPhotoXMPReceiptsSQL(), "photo_change_receipts_xmp_sidecar", int64(2)},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := s.writeDB.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { require.NoError(t, tx.Rollback()) }()

			rows, err := tx.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+test.query, test.argument)
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
			require.Contains(t, strings.Join(details, "\n"), "SEARCH r USING INDEX "+test.index+" (<expr>=?)")
		})
	}
}
