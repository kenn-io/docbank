package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkContentReferencesByHash measures a complete bounded page, including
// its total and paths, as the number of references to the same bytes grows.
func BenchmarkContentReferencesByHash(b *testing.B) {
	for _, files := range []int{100, 10000} {
		b.Run(fmt.Sprintf("files=%d", files), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			ctx := b.Context()
			hash := strings.Repeat("a1", 32)
			var first, last Node
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				for i := range files {
					node, _, err := s.createFileTx(ctx, tx, s.RootID(),
						fmt.Sprintf("file-%05d.bin", i), hash, 1024, "application/octet-stream")
					if err != nil {
						return err
					}
					if i == 0 {
						first = node
					}
					if i == 49 {
						last = node
					}
				}
				return nil
			}))
			b.ReportAllocs()
			for b.Loop() {
				refs, total, err := s.ContentReferencesByHash(ctx, hash, 50, 0)
				if err != nil || total != files || len(refs) != 50 {
					b.Fatalf("references=%d total=%d err=%v", len(refs), total, err)
				}
				if refs[0].Node.ID != first.ID || refs[49].Node.ID != last.ID ||
					refs[0].Version.ID != first.CurrentVersionID ||
					refs[49].Version.ID != last.CurrentVersionID ||
					refs[0].Path != "/file-00000.bin" || refs[49].Path != "/file-00049.bin" ||
					!refs[0].IsCurrent || !refs[49].IsCurrent {
					b.Fatal("content reference identities, paths, or current status changed")
				}
			}
		})
	}
}
