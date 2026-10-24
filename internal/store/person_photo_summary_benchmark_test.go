package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func BenchmarkPeopleRecentEvidenceHeavy(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, s.Close()) })
	ctx := b.Context()
	const people, photos = 3000, 40000
	versions := make([]string, photos)
	ids := make([]string, people)
	setup := time.Now()
	require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		for i := range photos {
			node, _, err := s.createFileTx(ctx, tx, s.RootID(), fmt.Sprintf("photo-%05d.jpg", i), fmt.Sprintf("%064x", i+1), 128, "image/jpeg")
			if err != nil {
				return err
			}
			if _, err := s.insertPhotoAssetTx(ctx, tx, node.ID, PhotoRoleImage, PhotoKindPhoto); err != nil {
				return err
			}
			versions[i] = node.CurrentVersionID
		}
		person, err := tx.PrepareContext(ctx, `INSERT INTO persons(person_id,display_name,display_name_folded,origin,state,created_at,updated_at) VALUES(?,?,?,'operator','curated',?,?)`)
		if err != nil {
			return err
		}
		defer func() { require.NoError(b, person.Close()) }()
		for i := range people {
			ids[i] = uuid.NewString()
			name := fmt.Sprintf("Person %05d", i)
			if _, err := person.ExecContext(ctx, ids[i], name, strings.ToLower(name), nowRFC3339(), nowRFC3339()); err != nil {
				return err
			}
		}
		assertion, err := tx.PrepareContext(ctx, `INSERT INTO person_document_assertions(assertion_id,content_version_id,person_id,role,action,note,recorded_at) VALUES(?,?,?,'depicted','assert','',?)`)
		if err != nil {
			return err
		}
		defer func() { require.NoError(b, assertion.Close()) }()
		start := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		for i, version := range versions {
			for k := range 3 {
				at := start.Add(time.Duration(i*3+k) * time.Microsecond).Format(timestampLayout)
				if _, err := assertion.ExecContext(ctx, uuid.NewString(), version, ids[(i*7+k*1009)%people], at); err != nil {
					return err
				}
			}
		}
		return nil
	}))
	b.Logf("fixture setup: %s", time.Since(setup))
	first, _, err := s.PeopleByDisplayName(ctx, "", "", "", 24, true, PeopleSearchOptions{RecentFirst: true})
	require.NoError(b, err)
	require.Len(b, first, 24)
	last := first[len(first)-1]
	for _, test := range []struct {
		name    string
		cursor  bool
		summary bool
	}{
		{name: "recent-visible"},
		{name: "recent-visible-cursor", cursor: true},
		{name: "recent-summary", summary: true},
	} {
		b.Run(test.name, func(b *testing.B) {
			options := PeopleSearchOptions{RecentFirst: true, PhotoSummary: test.summary, GridRecipe: "grid"}
			var afterName, afterID string
			if test.cursor {
				afterName, afterID, options.AfterRecent = last.DisplayNameFolded, last.PersonID, last.RecentRank
			}
			b.ReportAllocs()
			for b.Loop() {
				rows, _, err := s.PeopleByDisplayName(ctx, "", afterName, afterID, 24, true, options)
				if err != nil || len(rows) != 24 {
					b.Fatalf("rows=%d want=24 err=%v", len(rows), err)
				}
				if test.summary && (rows[0].PhotoSummary == nil || rows[0].PhotoSummary.LibraryCount == 0) {
					b.Fatal("missing card summary")
				}
			}
		})
	}
}

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
	const evidencePeople, photosPerPerson = 100, 250
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
	// Email contacts are senders of message versions, not photos.
	require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		for index := range 200 {
			if _, _, err := s.createFileTx(ctx, tx, s.RootID(), fmt.Sprintf("message-%03d.eml", index), fmt.Sprintf("%064x", heavyPhotos+index+1), 128, "message/rfc822"); err != nil {
				return err
			}
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
		// evidence. Reuse synthetic message versions to bound fixture setup and disk use.
		_, err = tx.ExecContext(ctx, `INSERT INTO person_document_assertions(assertion_id,content_version_id,person_id,role,action,note,recorded_at)
 SELECT persons.person_id || ':' || cv.version_id,cv.version_id,persons.person_id,'sender','assert','','2001-01-01T00:00:00.000000000Z'
 FROM (SELECT person_id FROM persons ORDER BY display_name_folded LIMIT ? OFFSET ?) persons
 CROSS JOIN (SELECT version_id FROM content_versions WHERE mime_type='message/rfc822' LIMIT 200) cv`, contacts, evidencePeople)
		return err
	}))
	// Tens of thousands of depicted assertions make the recent-first walk's
	// linear cost visible. Insert them in bulk with strictly increasing times.
	require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		face, err := tx.PrepareContext(ctx, `INSERT INTO photo_faces(face_id,content_version_id,x,y,width,height,revision,assertion_id,created_at) VALUES(?,?,.1,.1,.2,.2,1,?,?)`)
		if err != nil {
			return err
		}
		defer func() { require.NoError(b, face.Close()) }()
		assertion, err := tx.PrepareContext(ctx, `INSERT INTO person_document_assertions(assertion_id,content_version_id,person_id,role,action,note,recorded_at) VALUES(?,?,?,'depicted','assert','',?)`)
		if err != nil {
			return err
		}
		defer func() { require.NoError(b, assertion.Close()) }()
		start, count := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), 0
		for index := range evidencePeople {
			evidence := nodes[:photosPerPerson]
			if index < heavyPeople {
				evidence = nodes
			}
			for _, node := range evidence {
				at, assertionID := start.Add(time.Duration(count)*time.Microsecond).Format(timestampLayout), uuid.NewString()
				count++
				if _, err := assertion.ExecContext(ctx, assertionID, node.CurrentVersionID, ids[index], at); err != nil {
					return err
				}
				if _, err := face.ExecContext(ctx, uuid.NewString(), node.CurrentVersionID, assertionID, at); err != nil {
					return err
				}
			}
		}
		return nil
	}))
	// The tail case continues from a real page of every ranked group.
	ranked, _, err := s.PeopleByDisplayName(ctx, "", "", "", evidencePeople, true, PeopleSearchOptions{RecentFirst: true})
	require.NoError(b, err)
	require.Len(b, ranked, evidencePeople)
	for _, row := range ranked {
		require.NotEmpty(b, row.RecentRank)
	}
	last := ranked[len(ranked)-1]
	for _, test := range []struct {
		name         string
		query        string
		recentFirst  bool
		photoSummary bool
		limit        int
		rows         int // expected rows; zero means limit
		none         bool
		tail         bool
		unranked     bool
	}{
		{name: "page-24-name"},
		{name: "page-24-recent", recentFirst: true},
		{name: "page-24-name-summary", photoSummary: true},
		{name: "page-24-recent-summary", recentFirst: true, photoSummary: true},
		{name: "page-100-recent-prefix", query: "person 0000", recentFirst: true, limit: 100, rows: 10},
		{name: "page-100-recent-prefix-none", query: "nobody named this", recentFirst: true, limit: 100, none: true},
		{name: "page-24-recent-unranked-tail", recentFirst: true, tail: true, unranked: true},
	} {
		if test.limit == 0 {
			test.limit = 24
		}
		if test.rows == 0 && !test.none {
			test.rows = test.limit
		}
		options := PeopleSearchOptions{RecentFirst: test.recentFirst, PhotoSummary: test.photoSummary, GridRecipe: "grid"}
		var afterName, afterID string
		if test.tail {
			afterName, afterID, options.AfterRecent = last.DisplayNameFolded, last.PersonID, last.RecentRank
		}
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				rows, _, err := s.PeopleByDisplayName(ctx, test.query, afterName, afterID, test.limit, true, options)
				if err != nil || len(rows) != test.rows {
					b.Fatalf("rows=%d want=%d err=%v", len(rows), test.rows, err)
				}
				if len(rows) > 0 && test.recentFirst && (rows[0].RecentRank == "") != test.unranked {
					b.Fatalf("recent rank %q, unranked=%v", rows[0].RecentRank, test.unranked)
				}
				if test.photoSummary && (rows[0].PhotoSummary == nil || rows[0].PhotoSummary.LibraryCount == 0 || rows[0].PhotoSummary.Face == nil) {
					b.Fatal("missing card summary")
				}
			}
		})
	}
}
