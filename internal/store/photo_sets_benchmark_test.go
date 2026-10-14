package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// BenchmarkPhotoSetSummaries includes the read transaction, counts, and cover
// lookup. Synthetic album creation is outside the timer.
func BenchmarkPhotoSetSummaries(b *testing.B) {
	for _, shape := range []struct {
		albums, members int
		excluded        bool
	}{
		{1, 0, false}, {1, 10, false}, {1, 1000, false}, {1, 10000, false},
		{100, 0, false}, {100, 10, false}, {100, 100, false},
		{1, 1000, true}, {100, 100, true},
	} {
		b.Run(fmt.Sprintf("albums=%d/members=%d/excluded=%t", shape.albums, shape.members, shape.excluded), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			ctx := b.Context()
			var first string
			var covers []Node
			var coverIDs []string
			recipe := visualPreviewRecipe()
			_, fingerprint, err := document.MarshalVisualPreviewRecipeV1(recipe)
			require.NoError(b, err)
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				for album := range shape.albums {
					set, err := insertPhotoSet(ctx, tx, fmt.Sprintf("Album %03d", album))
					if err != nil {
						return err
					}
					if album == 0 {
						first = set.ID
					}
					for member := range shape.members {
						index := album*shape.members + member
						node, _, err := s.createFileTx(ctx, tx, s.RootID(),
							fmt.Sprintf("photo-%05d.jpg", index), fmt.Sprintf("%064x", index+1), 128, "image/jpeg")
						if err != nil {
							return err
						}
						asset, err := s.insertPhotoAssetTx(ctx, tx, node.ID, PhotoRoleImage, PhotoKindPhoto)
						if err != nil {
							return err
						}
						if _, err := tx.ExecContext(ctx, `INSERT INTO photo_set_members(set_id,asset_id,added_at) VALUES(?,?,?)`,
							set.ID, asset.ID, fmt.Sprintf("2026-01-01T00:00:00.%09dZ", member)); err != nil {
							return err
						}
						if member == shape.members-1 {
							covers = append(covers, node)
							coverIDs = append(coverIDs, asset.ID)
						}
					}
				}
				return nil
			}))
			included := shape.members
			if shape.excluded {
				_, err := s.db.ExecContext(ctx, `UPDATE photo_assets SET excluded_at='2026-01-01T00:00:00Z'`)
				require.NoError(b, err)
				included = 0
			}
			for _, ready := range []bool{false, true} {
				if ready && shape.members == 0 {
					continue
				}
				if ready {
					for _, node := range covers {
						canonical, _, err := document.MarshalVisualPreviewV1(document.VisualPreviewV1{
							ContractVersion: document.VisualPreviewContractV1, SourceSHA256: node.BlobHash,
							Recipe: recipe, State: document.VisualPreviewReady,
							Output: &document.VisualPreviewOutputV1{BlobSHA256: browseHash("synthetic-preview"), Size: 9, MediaType: "image/jpeg", Width: 100, Height: 100},
						})
						require.NoError(b, err)
						_, err = s.PublishVisualPreviewGeneration(ctx, node.CurrentVersionID, canonical, &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
						require.NoError(b, err)
					}
				}
				b.Run(fmt.Sprintf("ready=%t", ready), func(b *testing.B) {
					b.Run("list", func(b *testing.B) {
						b.ReportAllocs()
						for b.Loop() {
							sets, err := s.ListPhotoSets(ctx, fingerprint)
							if err != nil || len(sets) != shape.albums {
								b.Fatalf("albums=%d err=%v", len(sets), err)
							}
							for i, set := range sets {
								if set.MemberCount != int64(shape.members) || set.IncludedCount != int64(included) {
									b.Fatal("unexpected album counts")
								}
								if ready && !shape.excluded {
									if set.EffectiveCoverAssetID == nil || *set.EffectiveCoverAssetID != coverIDs[i] {
										b.Fatal("unexpected ready cover")
									}
								} else if set.EffectiveCoverAssetID != nil {
									b.Fatal("unexpected cover without a preview")
								}
							}
						}
					})
					b.Run("detail", func(b *testing.B) {
						b.ReportAllocs()
						for b.Loop() {
							set, err := s.PhotoSet(ctx, first, fingerprint)
							if err != nil || set.MemberCount != int64(shape.members) || set.IncludedCount != int64(included) {
								b.Fatalf("unexpected album summary: %+v err=%v", set, err)
							}
							if ready && !shape.excluded {
								if set.EffectiveCoverAssetID == nil || *set.EffectiveCoverAssetID != coverIDs[0] {
									b.Fatal("unexpected ready cover")
								}
							} else if set.EffectiveCoverAssetID != nil {
								b.Fatal("unexpected cover without a preview")
							}
						}
					})
				})
			}
		})
	}
}
