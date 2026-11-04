package store

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkAuditedMove measures a complete rename with retained trash elsewhere.
// Each case has nodes live files. Folder adds nodes inherited trash descendants;
// roots adds nodes/5 separately trashed files. Both are outside the moved subtree.
// Catalog creation and audit enrollment are outside the timer.
func BenchmarkAuditedMove(b *testing.B) {
	for _, nodes := range []int{1000, 10000, 100000} {
		for _, trash := range []string{"plain", "folder", "roots"} {
			b.Run(fmt.Sprintf("nodes=%d/trash=%s", nodes, trash), func(b *testing.B) {
				s := newTestStore(b)
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
					if trash != "plain" {
						count := nodes
						if trash == "roots" {
							count = nodes / 5
						}
						for i := range count {
							file, _, err := s.createFileTx(ctx, tx, kept.ID, fmt.Sprintf("trash-%06d.txt", i), fakeHash("b2"), 8, "text/plain")
							if err != nil {
								return err
							}
							if trash == "roots" {
								if err := s.trashNodeTx(ctx, tx, file, "2026-01-01T00:00:00.000000000Z"); err != nil {
									return err
								}
							}
						}
						if trash == "folder" {
							return s.trashNodeTx(ctx, tx, kept, "2026-01-01T00:00:00.000000000Z")
						}
					}
					return nil
				}))
				plan, err := s.PreviewInitialAudit(ctx, s.RootID(), "cli", nil)
				require.NoError(b, err)
				_, err = s.EnableInitialAudit(ctx, plan)
				require.NoError(b, err)
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
}
