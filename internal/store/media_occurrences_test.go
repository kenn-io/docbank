package store

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMediaRevokeIsPrincipalScopedAndIdempotent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, err := s.db.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`,
		strings.Repeat("a", 64), nowRFC3339())
	require.NoError(t, err)
	in := MediaOccurrenceInput{
		ID: "occ-a", SourceID: "source", Principal: "a", Ref: "r", Revision: "1", MessageJSON: "{}",
	}
	require.NoError(t, declareTestOccurrence(ctx, s, in))
	require.ErrorIs(t, revokeTestOccurrence(ctx, s, "b", "occ-a"), ErrNotFound)
	require.NoError(t, revokeTestOccurrence(ctx, s, "a", "occ-a"))
	require.NoError(t, revokeTestOccurrence(ctx, s, "a", "occ-a"))
	in.Revision = "2"
	in.ID = "occ-a-2"
	require.NoError(t, declareTestOccurrence(ctx, s, in))
	var visible int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM media_occurrences
		WHERE caller_principal='a' AND visible=1`).Scan(&visible))
	require.Equal(t, 1, visible)
}

func TestMediaOccurrenceRejectsChangedClaimsAndKeepsOtherPrincipalVisible(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, err := s.db.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`,
		strings.Repeat("a", 64), nowRFC3339())
	require.NoError(t, err)
	a := MediaOccurrenceInput{ID: "occ-a", SourceID: "source", Principal: "a", Ref: "same", Revision: "1", MessageJSON: "{}"}
	b := MediaOccurrenceInput{ID: "occ-b", SourceID: "source", Principal: "b", Ref: "same", Revision: "1", MessageJSON: "{}"}
	require.NoError(t, declareTestOccurrence(ctx, s, a))
	require.NoError(t, declareTestOccurrence(ctx, s, b))
	a.Filename = "changed.mp3"
	require.ErrorIs(t, declareTestOccurrence(ctx, s, a), ErrMediaOccurrenceConflict)
	require.NoError(t, revokeTestOccurrence(ctx, s, "a", "occ-a"))
	var visible int
	require.NoError(t, s.db.QueryRow(`SELECT visible FROM media_occurrences WHERE occurrence_id='occ-b'`).Scan(&visible))
	require.Equal(t, 1, visible)
}

func TestMediaOccurrenceConcurrentDeclarationCreatesOneRevision(t *testing.T) {
	t.Parallel()
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
			errs <- declareTestOccurrence(ctx, s, in)
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
}

func TestPublishMediaSourceVersionBindsOnlyUnboundOccurrences(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.CreateFile(ctx, s.RootID(), "first.mp3", fakeHash("a1"), 10, "audio/mpeg")
	require.NoError(t, err)
	second, err := s.CreateFile(ctx, s.RootID(), "second.mp3", fakeHash("b2"), 20, "audio/mpeg")
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`,
		strings.Repeat("a", 64), nowRFC3339())
	require.NoError(t, err)
	require.NoError(t, declareTestOccurrence(ctx, s, MediaOccurrenceInput{
		ID: "unbound", SourceID: "source", Principal: "a", Ref: "r", Revision: "1", MessageJSON: "{}",
	}))
	require.NoError(t, s.PublishMediaSourceVersion(ctx, MediaSourceVersionInput{
		ID: "version-1", SourceID: "source", Revision: 1, ContentVersionID: first.CurrentVersionID,
		CaptureJSON:          "{}",
		ExpectedHeadRevision: 0, BindOccurrenceIDs: []string{"unbound"},
	}))
	require.NoError(t, declareTestOccurrence(ctx, s, MediaOccurrenceInput{
		ID: "bound", SourceID: "source", SourceVersionID: "version-1", Principal: "a", Ref: "r", Revision: "2", MessageJSON: "{}",
	}))
	err = s.PublishMediaSourceVersion(ctx, MediaSourceVersionInput{
		ID: "version-2", SourceID: "source", Revision: 2, ContentVersionID: second.CurrentVersionID,
		CaptureJSON:          "{}",
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
	t.Parallel()
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
		CaptureJSON:      "[]",
	})
	require.ErrorIs(t, err, ErrMediaSourceConflict)
	var rows int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM media_source_versions`).Scan(&rows))
	require.Zero(t, rows)
	require.NoError(t, s.ExportMetadata(ctx, &bytes.Buffer{}))
}

func TestMediaOccurrenceMutationsRespectAuditedVaultGuard(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, err := s.db.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`,
		strings.Repeat("a", 64), nowRFC3339())
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())
	err = declareTestOccurrence(t.Context(), s, MediaOccurrenceInput{
		ID: "blocked", SourceID: "source", Principal: "a", Ref: "r", Revision: "1", MessageJSON: "{}",
	})
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
}

func declareTestOccurrence(ctx context.Context, s *Store, in MediaOccurrenceInput) error {
	_, err := s.RecordMediaOccurrence(ctx, testMediaOperation("declare_occurrence", in.Principal), in)
	return err
}

func revokeTestOccurrence(ctx context.Context, s *Store, principal, id string) error {
	var revision string
	_ = s.db.QueryRow(`SELECT caller_revision FROM media_occurrences WHERE occurrence_id=?`, id).Scan(&revision)
	_, err := s.RecordMediaOccurrenceRevocation(ctx, testMediaOperation("revoke_occurrence", principal), id, revision)
	return err
}

func testMediaOperation(verb, principal string) MediaOperation {
	id, _ := newUUIDv4()
	return MediaOperation{ID: id, Principal: principal, Verb: verb, RequestSHA256: strings.Repeat("a", 64)}
}
