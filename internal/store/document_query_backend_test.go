package store

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

func checkDocumentCatalogSnapshotPostgres(t *testing.T) {
	t.Helper()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	folder, err := s.Mkdir(ctx, s.RootID(), "before")
	require.NoError(t, err)
	for _, name := range []string{"a.txt", "B.txt", "é.txt"} {
		_, err := s.CreateFile(ctx, folder.ID, name, fakeHash(name), 4, "text/plain")
		require.NoError(t, err)
	}

	// Rename the selected folder and insert a document after path lookup.
	// A read-committed page would place the new document under the old path.
	config := s.postgresConfig.Copy()
	var writeErr error
	config.Tracer = &afterFirstSelect{run: func() {
		if _, _, err := s.Move(ctx, folder.ID, s.RootID(), "after", UnconditionalRev); err != nil {
			writeErr = err
			return
		}
		_, writeErr = s.CreateFile(ctx, folder.ID, "new.txt", fakeHash("new-catalog-file"), 5, "text/plain")
	}}
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	reader := &Store{db: db, rootID: s.RootID(), postgresSchema: s.postgresSchema}
	page, err := reader.ListDocuments(ctx, DocumentCatalogQuery{PathPrefix: "/before"}, nil, DocumentCatalogTraversalNext)
	require.NoError(t, writeErr)
	require.NoError(t, err)
	require.Equal(t, []string{"/before/B.txt", "/before/a.txt", "/before/é.txt"}, documentCatalogPaths(page.Items))

	page, err = s.ListDocuments(ctx, DocumentCatalogQuery{PathPrefix: "/after"}, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	require.Equal(t, []string{"/after/B.txt", "/after/a.txt", "/after/new.txt", "/after/é.txt"}, documentCatalogPaths(page.Items))
}

func checkDocumentCatalogSourcePresentPostgres(t *testing.T) {
	t.Helper()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	for _, name := range []string{"absent.txt", "present.txt"} {
		seedWatchRebind(t, s, name, name)
	}
	require.NoError(t, s.ReconcileWatchSourcePresence(ctx, "notes",
		WatchPresenceScan{ListedDirs: []string{""}, RegularFiles: []string{"present.txt"}}, time.Now()))
	page, err := s.ListDocuments(ctx, DocumentCatalogQuery{}, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	require.Equal(t, []string{"/absent.txt", "/present.txt"}, documentCatalogPaths(page.Items))
	page, err = s.ListDocuments(ctx, DocumentCatalogQuery{SourcePresent: true}, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	require.Equal(t, []string{"/present.txt"}, documentCatalogPaths(page.Items))
}
