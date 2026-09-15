package store

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMediaRevokeIsPrincipalScopedAndIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, err := s.db.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`,
		strings.Repeat("a", 64), nowRFC3339())
	require.NoError(t, err)
	in := MediaOccurrenceInput{
		ID: "occ-a", SourceID: "source", Principal: "a", Ref: "r", Revision: "1", MessageJSON: "{}",
	}
	require.NoError(t, s.DeclareMediaOccurrence(ctx, in))
	_, err = s.RevokeMediaOccurrence(ctx, "b", "occ-a")
	require.ErrorIs(t, err, ErrNotFound)
	first, err := s.RevokeMediaOccurrence(ctx, "a", "occ-a")
	require.NoError(t, err)
	second, err := s.RevokeMediaOccurrence(ctx, "a", "occ-a")
	require.NoError(t, err)
	require.Equal(t, first, second)
	in.Revision = "2"
	in.ID = "occ-a-2"
	require.NoError(t, s.DeclareMediaOccurrence(ctx, in))
	var visible int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM media_occurrences
		WHERE caller_principal='a' AND visible=1`).Scan(&visible))
	require.Equal(t, 1, visible)
}

func TestMediaOccurrenceRejectsChangedClaimsAndKeepsOtherPrincipalVisible(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, err := s.db.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`,
		strings.Repeat("a", 64), nowRFC3339())
	require.NoError(t, err)
	a := MediaOccurrenceInput{ID: "occ-a", SourceID: "source", Principal: "a", Ref: "same", Revision: "1", MessageJSON: "{}"}
	b := MediaOccurrenceInput{ID: "occ-b", SourceID: "source", Principal: "b", Ref: "same", Revision: "1", MessageJSON: "{}"}
	require.NoError(t, s.DeclareMediaOccurrence(ctx, a))
	require.NoError(t, s.DeclareMediaOccurrence(ctx, b))
	a.Filename = "changed.mp3"
	require.ErrorIs(t, s.DeclareMediaOccurrence(ctx, a), ErrMediaOccurrenceConflict)
	_, err = s.RevokeMediaOccurrence(ctx, "a", "occ-a")
	require.NoError(t, err)
	var visible int
	require.NoError(t, s.db.QueryRow(`SELECT visible FROM media_occurrences WHERE occurrence_id='occ-b'`).Scan(&visible))
	require.Equal(t, 1, visible)
}

func TestMediaOccurrenceConcurrentDeclarationCreatesOneRevision(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	_, err := s.db.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`,
		strings.Repeat("a", 64), nowRFC3339())
	require.NoError(t, err)
	in := MediaOccurrenceInput{ID: "occ", SourceID: "source", Principal: "a", Ref: "r", Revision: "1", MessageJSON: "{}"}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			<-start
			errs <- s.DeclareMediaOccurrence(ctx, in)
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var occurrences int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM media_occurrences`).Scan(&occurrences))
	require.Equal(t, 1, occurrences)
	var fence int64
	require.NoError(t, s.db.QueryRow(`SELECT fence FROM media_visibility_fences WHERE caller_principal='a'`).Scan(&fence))
	require.Equal(t, int64(1), fence)
}

func TestPublishMediaSourceVersionBindsOnlyUnboundOccurrences(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.CreateFile(ctx, s.RootID(), "first.mp3", fakeHash("a1"), 10, "audio/mpeg")
	require.NoError(t, err)
	second, err := s.CreateFile(ctx, s.RootID(), "second.mp3", fakeHash("b2"), 20, "audio/mpeg")
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`,
		strings.Repeat("a", 64), nowRFC3339())
	require.NoError(t, err)
	require.NoError(t, s.DeclareMediaOccurrence(ctx, MediaOccurrenceInput{
		ID: "unbound", SourceID: "source", Principal: "a", Ref: "r", Revision: "1", MessageJSON: "{}",
	}))
	require.NoError(t, s.PublishMediaSourceVersion(ctx, MediaSourceVersionInput{
		ID: "version-1", SourceID: "source", Revision: 1, ContentVersionID: first.CurrentVersionID,
		SourceSHA256: fakeHash("a1"), SourceBytes: 10, CaptureJSON: "{}", ClaimSHA256: digestCatalogJSON([]byte("{}")),
		ExpectedHeadRevision: 0, BindOccurrenceIDs: []string{"unbound"},
	}))
	require.NoError(t, s.DeclareMediaOccurrence(ctx, MediaOccurrenceInput{
		ID: "bound", SourceID: "source", SourceVersionID: "version-1", Principal: "a", Ref: "r", Revision: "2", MessageJSON: "{}",
	}))
	err = s.PublishMediaSourceVersion(ctx, MediaSourceVersionInput{
		ID: "version-2", SourceID: "source", Revision: 2, ContentVersionID: second.CurrentVersionID,
		SourceSHA256: fakeHash("b2"), SourceBytes: 20, CaptureJSON: "{}", ClaimSHA256: digestCatalogJSON([]byte("{}")),
		ExpectedHeadRevision: 1, BindOccurrenceIDs: []string{"bound"},
	})
	require.ErrorIs(t, err, ErrMediaOccurrenceConflict)
	var boundVersion string
	require.NoError(t, s.db.QueryRow(`SELECT source_version_id FROM media_occurrences WHERE occurrence_id='bound'`).Scan(&boundVersion))
	require.Equal(t, "version-1", boundVersion)
	var versions int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM media_source_versions`).Scan(&versions))
	require.Equal(t, 1, versions, "failed binding rolls back the new source version")
}

func TestPublishMediaSourceVersionRejectsNonObjectCaptureBeforeMutation(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	recording, err := s.CreateFile(ctx, s.RootID(), "recording.mp3", fakeHash("a1"), 10, "audio/mpeg")
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`,
		strings.Repeat("a", 64), nowRFC3339())
	require.NoError(t, err)

	err = s.PublishMediaSourceVersion(ctx, MediaSourceVersionInput{
		ID: "version-1", SourceID: "source", Revision: 1,
		ContentVersionID: recording.CurrentVersionID,
		SourceSHA256:     fakeHash("a1"), SourceBytes: 10,
		CaptureJSON: "[]", ClaimSHA256: digestCatalogJSON([]byte("[]")),
	})
	require.ErrorIs(t, err, ErrMediaSourceConflict)
	for _, table := range []string{"media_source_versions", "media_source_heads"} {
		var rows int
		require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM `+table).Scan(&rows))
		require.Zero(t, rows, table)
	}
	require.NoError(t, s.ExportMetadata(ctx, &bytes.Buffer{}))
}

func TestMediaOccurrenceMutationsRespectAuditedVaultGuard(t *testing.T) {
	s := newTestStore(t)
	_, err := s.db.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`,
		strings.Repeat("a", 64), nowRFC3339())
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())
	err = s.DeclareMediaOccurrence(t.Context(), MediaOccurrenceInput{
		ID: "blocked", SourceID: "source", Principal: "a", Ref: "r", Revision: "1", MessageJSON: "{}",
	})
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
}
