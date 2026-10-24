package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkTrashedRootsPage includes the count, page, and read transaction.
// Synthetic files and photo groups are created outside the timer.
func BenchmarkTrashedRootsPage(b *testing.B) {
	for _, groups := range []int{10, 1000, 10000} {
		for _, members := range []int{1, 3} {
			b.Run(fmt.Sprintf("groups=%d/members=%d", groups, members), func(b *testing.B) {
				s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
				require.NoError(b, err)
				b.Cleanup(func() { require.NoError(b, s.Close()) })
				ctx := b.Context()
				const stamp = "2026-01-01T00:00:00Z"
				representatives := make([]int64, 0, groups)
				require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
					for group := range groups {
						var asset PhotoAsset
						ext, mediaType := "bin", "application/octet-stream"
						if members > 1 {
							ext, mediaType = "jpg", "image/jpeg"
						}
						for member := range members {
							node, _, err := s.createFileTx(ctx, tx, s.RootID(),
								fmt.Sprintf("file-%05d-%d.%s", group, member, ext),
								fmt.Sprintf("%064x", group*members+member+1), 128, mediaType)
							if err != nil {
								return err
							}
							if err := s.trashNodeTx(ctx, tx, node, stamp); err != nil {
								return err
							}
							if member == 0 {
								representatives = append(representatives, node.ID)
							}
							if members == 1 {
								continue
							}
							if member == 0 {
								asset, err = s.insertPhotoAssetTx(ctx, tx, node.ID, PhotoRoleImage, PhotoKindPhoto)
							} else {
								err = s.insertPhotoFileTx(ctx, tx, PhotoFile{
									AssetID: asset.ID, NodeID: node.ID, Role: PhotoRoleImage, CreatedAt: stamp,
								})
							}
							if err != nil {
								return err
							}
						}
					}
					return nil
				}))
				for _, limit := range []int{50, 1000} {
					if limit == 1000 && groups < 1000 {
						continue
					}
					for _, offset := range slices.Compact([]int{0, max(0, groups-limit), groups}) {
						b.Run(fmt.Sprintf("limit=%d/offset=%d", limit, offset), func(b *testing.B) {
							wantCount := min(limit, groups-offset)
							wantMembers := 0
							if members > 1 {
								wantMembers = members
							}
							b.ReportAllocs()
							for b.Loop() {
								page, total, err := s.TrashedRootsPage(ctx, limit, offset)
								if err != nil || total != groups || len(page) != wantCount {
									b.Fatalf("items=%d total=%d err=%v", len(page), total, err)
								}
								for i, item := range page {
									if item.ID != representatives[groups-offset-i-1] || item.PhotoFileCount != wantMembers {
										b.Fatal("unexpected trash representative or member count")
									}
								}
							}
						})
					}
				}
			})
		}
	}
}

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
