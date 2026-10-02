package store

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkResolveIngestName isolates the work performed for every local add
// and HTTP upload as the destination directory grows. Setup uses real file
// authority but is excluded from timing.
func BenchmarkResolveIngestName(b *testing.B) {
	for _, siblings := range []int{0, 100, 1000, 10000} {
		b.Run(fmt.Sprintf("siblings=%d", siblings), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			for i := range siblings {
				_, err := s.CreateFile(b.Context(), s.RootID(), fmt.Sprintf("file-%06d.bin", i),
					fmt.Sprintf("%064x", i+1), 4096, "application/octet-stream")
				require.NoError(b, err)
			}
			tx, err := s.writeDB.BeginTx(b.Context(), nil)
			require.NoError(b, err)
			defer func() { require.NoError(b, tx.Rollback()) }()
			b.ReportAllocs()
			for b.Loop() {
				name, _, skipped, err := resolveIngestNameTx(tx, s.RootID(), "new.bin",
					fmt.Sprintf("%064x", siblings+1), "cli")
				require.NoError(b, err)
				require.False(b, skipped)
				require.Equal(b, "new.bin", name)
			}
		})
	}
}
