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

// BenchmarkPeopleRecentAtVaultScale compares a name page with recency ranking
// and card summaries, including people with hundreds of photos.
func BenchmarkPeopleRecentAtVaultScale(b *testing.B) {
	const people, evidencePeople, photosPerPerson = 5000, 100, 3
	const heavyPeople, heavyPhotos = 8, 300
	s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, s.Close()) })
	ctx := b.Context()
	var nodes []Node
	require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		for index := range heavyPhotos {
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
		person, err := s.CreatePerson(ctx, fmt.Sprintf("Person %05d", index), "operator")
		require.NoError(b, err)
		if index >= evidencePeople {
			continue
		}
		evidence := nodes[:photosPerPerson]
		if index < heavyPeople {
			evidence = nodes
		}
		for _, node := range evidence {
			_, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .1, Y: .1, Width: .2, Height: .2}, person.PersonID)
			require.NoError(b, err)
		}
	}
	for _, test := range []struct {
		name         string
		recentFirst  bool
		photoSummary bool
	}{
		{name: "page-24-name"},
		{name: "page-24-recent", recentFirst: true},
		{name: "page-24-name-summary", photoSummary: true},
		{name: "page-24-recent-summary", recentFirst: true, photoSummary: true},
	} {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				rows, err := s.PeopleByDisplayName(ctx, "", "", "", 24, true, PeopleSearchOptions{RecentFirst: test.recentFirst, PhotoSummary: test.photoSummary, GridRecipe: "grid"})
				if err != nil || len(rows) != 24 || test.recentFirst && rows[0].RecentRank == "" {
					b.Fatalf("rows=%d err=%v", len(rows), err)
				}
				if test.photoSummary && (rows[0].PhotoSummary == nil || rows[0].PhotoSummary.LibraryCount == 0 || rows[0].PhotoSummary.Face == nil) {
					b.Fatal("missing card summary")
				}
			}
		})
	}
}
