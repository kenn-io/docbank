package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkAuditedMove measures a complete rename with retained trash elsewhere.
// Catalog creation and audit enrollment are outside the timer.
func BenchmarkAuditedMove(b *testing.B) {
	for _, nodes := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("nodes=%d", nodes), func(b *testing.B) {
			s, err := Open(b.Context(), filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			ctx := b.Context()
			projects, _, err := s.MkdirPath(ctx, "/Projects")
			require.NoError(b, err)
			work, err := s.Mkdir(ctx, projects.ID, "Work")
			require.NoError(b, err)
			kept, err := s.Mkdir(ctx, s.RootID(), "Kept")
			require.NoError(b, err)
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				for folder := range 100 {
					dir, err := s.mkdirTx(ctx, tx, s.RootID(), fmt.Sprintf("folder-%03d", folder), "2026-01-01T00:00:00.000000000Z")
					if err != nil {
						return err
					}
					for i := range nodes / 100 {
						if _, _, err := s.createFileTx(ctx, tx, dir.ID,
							fmt.Sprintf("file-%06d.txt", i), fmt.Sprintf("%064x", folder*nodes+i+1), 8, "text/plain"); err != nil {
							return err
						}
					}
				}
				return nil
			}))
			plan, err := s.PreviewInitialAudit(ctx, s.RootID(), "cli", nil)
			require.NoError(b, err)
			_, err = s.EnableInitialAudit(ctx, plan)
			require.NoError(b, err)
			for i := range 10 {
				file, err := s.CreateFile(ctx, kept.ID, fmt.Sprintf("retained-%d.txt", i), fakeHash("b1"), 8, "text/plain")
				require.NoError(b, err)
				_, _, err = s.Trash(ctx, file.ID, UnconditionalRev)
				require.NoError(b, err)
			}
			name := "Renamed"
			b.ReportAllocs()
			for b.Loop() {
				moved, path, err := s.Move(ctx, work.ID, projects.ID, name, UnconditionalRev)
				if err != nil || moved.ID != work.ID || path != "/Projects/"+name {
					b.Fatalf("unexpected move: node=%d path=%q err=%v", moved.ID, path, err)
				}
				if name == "Renamed" {
					name = "Work"
				} else {
					name = "Renamed"
				}
			}
		})
	}
}
