package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkRestoreNameCollision measures restore's name selection inside its
// transaction. Synthetic catalog creation and transaction setup are excluded.
func BenchmarkRestoreNameCollision(b *testing.B) {
	for _, conflicts := range []int{0, 1, 10, 1000, 10000} {
		b.Run(fmt.Sprintf("conflicts=%d", conflicts), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			require.NoError(b, s.withStorageTx(b.Context(), func(tx *sql.Tx) error {
				for i := range conflicts {
					name := "report.pdf"
					if i > 0 {
						name = fmt.Sprintf("report (%d).pdf", i+1)
					}
					if _, err := s.mkdirTx(tx, s.RootID(), name, "2026-01-01T00:00:00Z"); err != nil {
						return err
					}
				}
				return nil
			}))
			tx, err := s.writeDB.BeginTx(b.Context(), nil)
			require.NoError(b, err)
			defer func() { require.NoError(b, tx.Rollback()) }()
			want := "report.pdf"
			if conflicts > 0 {
				want = fmt.Sprintf("report (%d).pdf", conflicts+1)
			}
			b.ReportAllocs()
			for b.Loop() {
				name, err := nextFreeNameTx(b.Context(), tx, s.RootID(), "report.pdf")
				if err != nil || name != want {
					b.Fatalf("name=%q, want %q, err=%v", name, want, err)
				}
			}
		})
	}
}
