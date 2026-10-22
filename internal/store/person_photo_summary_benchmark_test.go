package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// BenchmarkPeoplePhotoSummaries reads one People page with counts and
// portraits. Every person is linked to every synthetic photo, and each has one
// boxed face. Fixture creation is outside the timer.
func BenchmarkPeoplePhotoSummaries(b *testing.B) {
	const photos = 5000
	for _, people := range []int{24, 100} {
		b.Run(fmt.Sprintf("people=%d/photos=%d", people, photos), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			ctx := b.Context()
			var nodes []Node
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				for index := range photos {
					node, _, err := s.createFileTx(ctx, tx, s.RootID(), fmt.Sprintf("photo-%05d.jpg", index), fmt.Sprintf("%064x", index+1), 128, "image/jpeg")
					if err != nil {
						return err
					}
					if _, err := s.insertPhotoAssetTx(ctx, tx, node.ID, PhotoRoleImage, PhotoKindPhoto); err != nil {
						return err
					}
					nodes = append(nodes, node)
				}
				return nil
			}))
			for index := range people {
				person, err := s.CreatePerson(ctx, fmt.Sprintf("Person %03d", index), "operator")
				require.NoError(b, err)
				_, _, err = s.CreatePhotoFace(ctx, nodes[index].CurrentVersionID, uuid.NewString(), document.FaceBox{X: .1, Y: .1, Width: .2, Height: .2}, person.PersonID)
				require.NoError(b, err)
			}
			_, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO person_document_assertions(assertion_id,content_version_id,person_id,role,action,note,recorded_at)
				SELECT lower(hex(randomblob(16))),n.current_version_id,p.person_id,'depicted','assert','','2026-01-01T00:00:00Z'
				FROM nodes n CROSS JOIN persons p WHERE n.current_version_id IS NOT NULL`)
			require.NoError(b, err)
			for _, recent := range []bool{false, true} {
				b.Run(fmt.Sprintf("recent=%t", recent), func(b *testing.B) {
					options := PeopleSearchOptions{RecentFirst: recent, PhotoSummary: true, GridRecipe: "grid"}
					b.ReportAllocs()
					for b.Loop() {
						rows, err := s.PeopleByDisplayName(ctx, "", "", "", people, true, options)
						if err != nil || len(rows) != people || rows[0].PhotoSummary.LibraryCount != photos || rows[0].PhotoSummary.Face == nil {
							b.Fatalf("rows=%d err=%v", len(rows), err)
						}
					}
				})
			}
		})
	}
}
