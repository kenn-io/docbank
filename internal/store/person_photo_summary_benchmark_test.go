package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// BenchmarkPeopleRecentAtVaultScale compares a name page with recency ranking
// and card summaries, including people with hundreds of photos.
func BenchmarkPeopleRecentAtVaultScale(b *testing.B) {
	for _, scale := range []struct {
		name     string
		people   int
		contacts int
	}{
		{name: "5000-people", people: 5000},
		{name: "50000-people-mail-heavy", people: 50000, contacts: 10000},
		{name: "100000-people", people: 100000},
	} {
		b.Run(scale.name, func(b *testing.B) {
			benchmarkPeopleRecent(b, scale.people, scale.contacts)
		})
	}
}

func benchmarkPeopleRecent(b *testing.B, people, contacts int) {
	b.Helper()
	const evidencePeople, photosPerPerson = 100, 3
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
	ids := make([]string, people)
	require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		insert, err := tx.PrepareContext(ctx, `INSERT INTO persons(person_id,display_name,display_name_folded,origin,state,created_at,updated_at) VALUES(?,?,?,'operator','curated',?,?)`)
		if err != nil {
			return err
		}
		defer func() { require.NoError(b, insert.Close()) }()
		for index := range people {
			ids[index] = uuid.NewString()
			name := fmt.Sprintf("Person %05d", index)
			if _, err := insert.ExecContext(ctx, ids[index], name, strings.ToLower(name), nowRFC3339(), nowRFC3339()); err != nil {
				return err
			}
		}
		// Email contacts have hundreds of sender assertions but no depicted
		// evidence. Reuse synthetic versions to bound fixture setup and disk use.
		_, err = tx.ExecContext(ctx, `INSERT INTO person_document_assertions(assertion_id,content_version_id,person_id,role,action,note,recorded_at)
 SELECT persons.person_id || ':' || cv.version_id,cv.version_id,persons.person_id,'sender','assert','','2001-01-01T00:00:00.000000000Z'
 FROM (SELECT person_id FROM persons ORDER BY display_name_folded LIMIT ? OFFSET ?) persons
 CROSS JOIN (SELECT version_id FROM content_versions LIMIT 200) cv`, contacts, evidencePeople)
		return err
	}))
	for index := range evidencePeople {
		evidence := nodes[:photosPerPerson]
		if index < heavyPeople {
			evidence = nodes
		}
		for _, node := range evidence {
			_, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .1, Y: .1, Width: .2, Height: .2}, ids[index])
			require.NoError(b, err)
		}
	}
	for _, test := range []struct {
		name         string
		recentFirst  bool
		photoSummary bool
		limit        int
	}{
		{name: "page-25-default", limit: 25},
		{name: "page-24-name"},
		{name: "page-24-recent", recentFirst: true},
		{name: "page-24-name-summary", photoSummary: true},
		{name: "page-24-recent-summary", recentFirst: true, photoSummary: true},
	} {
		if test.limit == 0 {
			test.limit = 24
		}
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				rows, _, err := s.PeopleByDisplayName(ctx, "", "", "", test.limit, true, PeopleSearchOptions{RecentFirst: test.recentFirst, PhotoSummary: test.photoSummary, GridRecipe: "grid"})
				if err != nil || len(rows) != test.limit || test.recentFirst && rows[0].RecentRank == "" {
					b.Fatalf("rows=%d err=%v", len(rows), err)
				}
				if test.photoSummary && (rows[0].PhotoSummary == nil || rows[0].PhotoSummary.LibraryCount == 0 || rows[0].PhotoSummary.Face == nil) {
					b.Fatal("missing card summary")
				}
			}
		})
	}
}
