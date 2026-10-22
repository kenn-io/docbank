package store

import (
	"cmp"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// BenchmarkPeopleSparseEvidence reads People cards and ranks a typed prefix
// while unrelated photos grow. Each of 100 people has a boxed face in three
// photos; the other photos depict no one. A query that starts from the
// requested people's assertions should not slow down as unrelated photos grow.
// Fixture creation is outside the timer.
func BenchmarkPeopleSparseEvidence(b *testing.B) {
	const people, perPerson = 100, 3
	for _, unrelated := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("unrelated=%d", unrelated), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			ctx := b.Context()
			var nodes []Node
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				for index := range people*perPerson + unrelated {
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
			var first string
			for index := range people {
				person, err := s.CreatePerson(ctx, fmt.Sprintf("Person %03d", index), "operator")
				require.NoError(b, err)
				first = cmp.Or(first, person.PersonID)
				for photo := range perPerson {
					_, _, err = s.CreatePhotoFace(ctx, nodes[index*perPerson+photo].CurrentVersionID, uuid.NewString(), document.FaceBox{X: .1, Y: .1, Width: .2, Height: .2}, person.PersonID)
					require.NoError(b, err)
				}
			}
			cases := []struct {
				name  string
				query string
				limit int
				want  int
				opts  PeopleSearchOptions
			}{
				{"refresh-one", "", 1, 1, PeopleSearchOptions{PersonID: first, PhotoSummary: true, GridRecipe: "grid"}},
				{"page-24", "", 24, 24, PeopleSearchOptions{PhotoSummary: true, GridRecipe: "grid"}},
				{"prefix-recent", "Person 05", 24, 10, PeopleSearchOptions{RecentFirst: true}},
			}
			for _, c := range cases {
				b.Run(c.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						rows, err := s.PeopleByDisplayName(ctx, c.query, "", "", c.limit, true, c.opts)
						if err != nil || len(rows) != c.want || c.opts.PhotoSummary && (rows[0].PhotoSummary.LibraryCount != perPerson || rows[0].PhotoSummary.Face == nil) ||
							c.opts.RecentFirst && rows[0].RecentRank == "" {
							b.Fatalf("rows=%d err=%v", len(rows), err)
						}
					}
				})
			}
		})
	}
}
