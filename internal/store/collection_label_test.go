package store

import (
	"database/sql"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectionLabelFence(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "cli", "Synthetic import")
	require.NoError(t, err)
	_, _, err = s.IngestFile(ctx, run, s.RootID(), "note.txt", fakeHash("a1"),
		4, "text/plain", "/synthetic/note.txt", "")
	require.NoError(t, err)

	initial, err := s.CollectionLabel(ctx, run.ID())
	require.NoError(t, err)
	assert.Equal(t, CollectionLabel{
		IngestID: run.ID(), Revision: 1, UpdatedAt: run.record.StartedAt,
	}, initial)

	name := "Review set"
	label, err := s.SetCollectionLabel(ctx, run.ID(), 1, &name)
	require.NoError(t, err)
	require.Equal(t, int64(2), label.Revision)
	other := "Changed set"
	_, err = s.SetCollectionLabel(ctx, run.ID(), 1, &other)
	require.ErrorIs(t, err, ErrStaleRevision)
	current, err := s.CollectionLabel(ctx, run.ID())
	require.NoError(t, err)
	require.Equal(t, label, current)
}

func TestCollectionLabelValidationAndCaseSensitiveUniqueness(t *testing.T) {
	s := newTestStore(t)
	first := createCollectionRun(t, s, "first.txt", "a1")
	second := createCollectionRun(t, s, "second.txt", "b2")

	for _, test := range []struct {
		name  string
		label string
	}{
		{name: "empty", label: ""},
		{name: "whitespace", label: " \t "},
		{name: "control", label: "bad\nlabel"},
		{name: "too many bytes", label: string(make([]byte, 257))},
		{name: "invalid UTF-8", label: string([]byte{0xff})},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := s.SetCollectionLabel(t.Context(), first.ID(), 1, &test.label)
			require.Error(t, err)
		})
	}

	nfd := "Cafe\u0301 records"
	got, err := s.SetCollectionLabel(t.Context(), first.ID(), 1, &nfd)
	require.NoError(t, err)
	require.NotNil(t, got.Label)
	assert.Equal(t, "Café records", *got.Label)

	differentCase := "CAFÉ records"
	_, err = s.SetCollectionLabel(t.Context(), second.ID(), 1, &differentCase)
	require.NoError(t, err)
	collision := "Café records"
	_, err = s.SetCollectionLabel(t.Context(), second.ID(), 2, &collision)
	require.ErrorIs(t, err, ErrExists)

	spaced := "  kept spaces  "
	cleared, err := s.SetCollectionLabel(t.Context(), first.ID(), 2, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(3), cleared.Revision)
	spacedLabel, err := s.SetCollectionLabel(t.Context(), first.ID(), 3, &spaced)
	require.NoError(t, err)
	require.Equal(t, spaced, *spacedLabel.Label)
}

func TestCollectionLabelNoOpClearAndRetainedEmptyAuthority(t *testing.T) {
	s := newTestStore(t)
	run := createCollectionRun(t, s, "note.txt", "a1")

	virtual, err := s.SetCollectionLabel(t.Context(), run.ID(), 1, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(1), virtual.Revision)
	var rows int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM collection_labels`).Scan(&rows))
	assert.Zero(t, rows)

	name := "Review set"
	created, err := s.SetCollectionLabel(t.Context(), run.ID(), 1, &name)
	require.NoError(t, err)
	unchanged, err := s.SetCollectionLabel(t.Context(), run.ID(), created.Revision, &name)
	require.NoError(t, err)
	assert.Equal(t, created, unchanged)

	page, err := s.CollectionMembers(t.Context(), run.ID(), 10, 0)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), page.Items[0].Node.ID, UnconditionalRev)
	require.NoError(t, err)
	collections, total, err := s.Collections(t.Context(), 100, 0)
	require.NoError(t, err)
	assert.Empty(t, collections)
	assert.Zero(t, total)
	empty, err := s.CollectionByID(t.Context(), run.ID())
	require.NoError(t, err)
	assert.Zero(t, empty.FileCount)
	assert.Equal(t, created.Revision, empty.LabelRevision)
	other := createCollectionRun(t, s, "other.txt", "b2")
	_, err = s.SetCollectionLabel(t.Context(), other.ID(), 1, &name)
	require.ErrorIs(t, err, ErrExists,
		"a retained empty collection keeps its non-null label unique")

	cleared, err := s.SetCollectionLabel(t.Context(), run.ID(), created.Revision, nil)
	require.NoError(t, err)
	assert.Equal(t, created.Revision+1, cleared.Revision)
	assert.Nil(t, cleared.Label)
	again, err := s.SetCollectionLabel(t.Context(), run.ID(), cleared.Revision, nil)
	require.NoError(t, err)
	assert.Equal(t, cleared, again)
	reused, err := s.SetCollectionLabel(t.Context(), other.ID(), 1, &name)
	require.NoError(t, err)
	require.Equal(t, name, *reused.Label)
	_, err = s.CollectionLabel(t.Context(), "not-a-uuid")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestCollectionLabelRejectsArbitraryEmptyIngest(t *testing.T) {
	s := newTestStore(t)
	run, err := s.BeginIngest(t.Context(), "cli", "No committed documents")
	require.NoError(t, err)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := ensureIngestRunTx(t.Context(), tx, run)
		return err
	}))

	_, err = s.CollectionLabel(t.Context(), run.ID())
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.SetCollectionLabel(t.Context(), run.ID(), 1, new("Invented"))
	require.ErrorIs(t, err, ErrNotFound)
}

func TestCollectionLabelTimestampNeverMovesBackward(t *testing.T) {
	s := newTestStore(t)
	run := createCollectionRun(t, s, "note.txt", "a1")
	created, err := s.SetCollectionLabel(t.Context(), run.ID(), 1, new("First"))
	require.NoError(t, err)
	const future = "2099-01-02T03:04:05.000000000Z"
	_, err = s.db.Exec(`UPDATE collection_labels SET updated_at=? WHERE ingest_id=?`, future, run.ID())
	require.NoError(t, err)

	updated, err := s.SetCollectionLabel(t.Context(), run.ID(), created.Revision, new("Second"))
	require.NoError(t, err)
	assert.Equal(t, future, updated.UpdatedAt)
}

func TestCollectionLabelFirstWriteRaceHasOneWinner(t *testing.T) {
	s := newTestStore(t)
	run := createCollectionRun(t, s, "note.txt", "a1")
	names := []string{"First", "Second"}
	errs := make([]error, 2)
	labels := make([]CollectionLabel, 2)
	var start sync.WaitGroup
	start.Add(1)
	var workers sync.WaitGroup
	for i := range names {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			start.Wait()
			labels[i], errs[i] = s.SetCollectionLabel(t.Context(), run.ID(), 1, &names[i])
		}(i)
	}
	start.Done()
	workers.Wait()

	winners := 0
	stale := 0
	for i := range errs {
		if errs[i] == nil {
			winners++
			assert.Equal(t, int64(2), labels[i].Revision)
		} else if assert.ErrorIs(t, errs[i], ErrStaleRevision) {
			stale++
		}
	}
	assert.Equal(t, 1, winners)
	assert.Equal(t, 1, stale)
}

func TestBeginIngestWithLabelPublishesAtomically(t *testing.T) {
	s := newTestStore(t)
	first := createCollectionRun(t, s, "first.txt", "a1")
	name := "Shared label"
	_, err := s.SetCollectionLabel(t.Context(), first.ID(), 1, &name)
	require.NoError(t, err)

	second, err := s.BeginIngestWithLabel(t.Context(), "cli", "Collision", &name)
	require.NoError(t, err)
	var unpublished int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM ingests WHERE id=?`, second.ID()).Scan(&unpublished))
	assert.Zero(t, unpublished, "preparing a labeled run grants no database authority")
	_, _, err = s.IngestFile(t.Context(), second, s.RootID(), "second.txt", fakeHash("b2"),
		8, "text/plain", "/synthetic/second.txt", "")
	require.ErrorIs(t, err, ErrExists)
	var ingests, nodes, blobs int
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM ingests WHERE id=?),
		(SELECT COUNT(*) FROM nodes WHERE name='second.txt'),
		(SELECT COUNT(*) FROM blobs WHERE hash=?)`, second.ID(), fakeHash("b2")).Scan(&ingests, &nodes, &blobs))
	assert.Zero(t, ingests)
	assert.Zero(t, nodes)
	assert.Zero(t, blobs)

	initialName := "Initial label"
	third, err := s.BeginIngestWithLabel(t.Context(), "cli", "Initial", &initialName)
	require.NoError(t, err)
	_, _, err = s.IngestFile(t.Context(), third, s.RootID(), "third.txt", fakeHash("c3"),
		9, "text/plain", "/synthetic/third.txt", "")
	require.NoError(t, err)
	label, err := s.CollectionLabel(t.Context(), third.ID())
	require.NoError(t, err)
	assert.Equal(t, int64(1), label.Revision)
	assert.Equal(t, third.record.StartedAt, label.UpdatedAt)
	require.Equal(t, initialName, *label.Label)

	edited := "Edited label"
	updated, err := s.SetCollectionLabel(t.Context(), third.ID(), 1, &edited)
	require.NoError(t, err)
	_, _, err = s.IngestFile(t.Context(), third, s.RootID(), "fourth.txt", fakeHash("d4"),
		10, "text/plain", "/synthetic/fourth.txt", "")
	require.NoError(t, err)
	current, err := s.CollectionLabel(t.Context(), third.ID())
	require.NoError(t, err)
	assert.Equal(t, updated, current)
}

func TestBeginIngestWithLabelRejectsCallerSuppliedSourceKind(t *testing.T) {
	s := newTestStore(t)
	for _, sourceKind := range []string{"embedded:cli", "EMBEDDED:cli", "EmBeDdEd:watch"} {
		t.Run(sourceKind, func(t *testing.T) {
			_, err := s.BeginIngestWithLabel(
				t.Context(), sourceKind, "Opaque", new("Hidden"),
			)
			require.ErrorIs(t, err, ErrInvalidCollectionLabel)
			var ingests, labels int
			require.NoError(t, s.db.QueryRow(`SELECT
				(SELECT COUNT(*) FROM ingests),
				(SELECT COUNT(*) FROM collection_labels)`).Scan(&ingests, &labels))
			require.Zero(t, ingests)
			require.Zero(t, labels)
		})
	}
}

func TestCollectionLabelsRejectAllMutationsUnderAudit(t *testing.T) {
	s := newTestStore(t)
	retained := createCollectionRun(t, s, "retained.txt", "a1")
	name := "Retained"
	_, err := s.SetCollectionLabel(t.Context(), retained.ID(), 1, &name)
	require.NoError(t, err)
	unlabeled := createCollectionRun(t, s, "unlabeled.txt", "b2")
	plan, err := s.PreviewInitialAudit(t.Context(), s.RootID(), "api", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)

	for _, mutation := range []struct {
		name     string
		ingestID string
		revision int64
		label    *string
	}{
		{name: "create", ingestID: unlabeled.ID(), revision: 1, label: new("New")},
		{name: "edit", ingestID: retained.ID(), revision: 2, label: new("Changed")},
		{name: "clear", ingestID: retained.ID(), revision: 2},
		{name: "no-op", ingestID: retained.ID(), revision: 2, label: &name},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			_, err := s.SetCollectionLabel(t.Context(), mutation.ingestID, mutation.revision, mutation.label)
			require.ErrorIs(t, err, ErrAuditMutationUnsupported)
		})
	}

	initial := "Rejected initial"
	run, err := s.BeginIngestWithLabel(t.Context(), "cli", "Audited", &initial)
	require.NoError(t, err)
	_, _, err = s.IngestFile(t.Context(), run, s.RootID(), "rejected.txt", fakeHash("c3"),
		3, "text/plain", "/synthetic/rejected.txt", "")
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	var authority int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM ingests WHERE id=?`, run.ID()).Scan(&authority))
	assert.Zero(t, authority)
	current, err := s.CollectionLabel(t.Context(), retained.ID())
	require.NoError(t, err)
	require.Equal(t, name, *current.Label)
}

func createCollectionRun(t *testing.T, s *Store, fileName, hashSeed string) IngestRun {
	t.Helper()
	run, err := s.BeginIngest(t.Context(), "cli", "Synthetic "+fileName)
	require.NoError(t, err)
	_, _, err = s.IngestFile(t.Context(), run, s.RootID(), fileName, fakeHash(hashSeed),
		4, "text/plain", "/synthetic/"+fileName, "")
	require.NoError(t, err)
	return run
}
