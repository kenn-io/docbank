package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOperationalReimportKeepsVersionAndAddsMembership(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	firstRun, err := s.BeginIngest(ctx, "cli", "First synthetic import")
	require.NoError(t, err)
	first, _, err := s.IngestFile(ctx, firstRun, s.RootID(), "note.txt", fakeHash("a1"),
		4, "text/plain", "/synthetic/note.txt", "")
	require.NoError(t, err)
	secondRun, err := s.BeginIngest(ctx, "cli", "Second synthetic import")
	require.NoError(t, err)
	second, added, err := s.IngestFileWithMembership(ctx, secondRun, s.RootID(), "note.txt",
		fakeHash("a1"), 4, "text/plain", "/synthetic/note.txt", "")
	require.NoError(t, err)
	require.False(t, added)
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, first.CurrentVersionID, second.CurrentVersionID)
	require.Equal(t, first.BlobHash, second.BlobHash)
	require.Equal(t, first.Revision+1, second.Revision)
	for _, id := range []string{firstRun.ID(), secondRun.ID()} {
		collection, err := s.CollectionByID(ctx, id)
		require.NoError(t, err)
		require.Equal(t, int64(1), collection.FileCount)
	}

	replayed, replayAdded, err := s.IngestFileWithMembership(ctx, secondRun, s.RootID(), "note.txt",
		fakeHash("a1"), 4, "text/plain", "/synthetic/note.txt", "")
	require.NoError(t, err)
	assert.False(t, replayAdded)
	assert.Equal(t, second.Revision, replayed.Revision)
	var facts int64
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM provenance WHERE node_id=? AND ingest_id=?`, second.ID, secondRun.ID(),
	).Scan(&facts))
	assert.Equal(t, int64(1), facts)
}

func TestMembershipIngestSourceKinds(t *testing.T) {
	for _, kind := range []string{"cli", "upload", "filesystem", "custom-source", "watch"} {
		for _, operation := range []string{"create", "reimport", "replace", "planned", "planned exact"} {
			t.Run(kind+"/"+operation, func(t *testing.T) {
				s := newTestStore(t)
				ctx := t.Context()
				seed, err := s.BeginIngest(ctx, kind, "synthetic-initial-import")
				require.NoError(t, err)
				prior, _, err := s.IngestFile(ctx, seed, s.RootID(), "note.txt", fakeHash("a1"),
					4, "text/plain", "note.txt", "")
				require.NoError(t, err)
				var label *string
				if operation == "planned" || operation == "planned exact" {
					label = new("Synthetic import")
				}
				run, err := s.BeginIngestWithLabel(ctx, kind, "synthetic-import", label)
				require.NoError(t, err)
				plan, err := s.PrepareIngestDirectory(ctx, "/new")
				require.NoError(t, err)
				before := exportStoreMetadata(t, s)
				switch operation {
				case "create":
					_, _, err = s.IngestFileWithMembership(ctx, run, s.RootID(), "new.txt",
						fakeHash("b2"), 8, "text/plain", "new.txt", "")
				case "reimport":
					_, _, err = s.IngestFileWithMembership(ctx, run, s.RootID(), "note.txt",
						fakeHash("a1"), 4, "text/plain", "note.txt", "")
				case "replace":
					_, _, err = s.ReplaceContentForIngest(ctx, run, prior.ID, prior.Revision,
						fakeHash("b2"), 8, "text/plain", "note.txt", "")
				case "planned":
					_, _, _, err = s.IngestFileWithMembershipPlanned(ctx, run, plan, "new.txt",
						fakeHash("b2"), 8, "text/plain", "new.txt", "")
				case "planned exact":
					_, _, err = s.IngestFileExactPlanned(ctx, run, plan, "new.txt",
						fakeHash("b2"), 8, "text/plain", "new.txt", "")
				}
				require.NoError(t, s.ValidateMetadata(ctx))
				if kind == "watch" {
					require.ErrorContains(t, err, "rejects source kind")
					assert.Equal(t, before, exportStoreMetadata(t, s))
				} else {
					require.NoError(t, err)
					collection, err := s.CollectionByID(ctx, run.ID())
					require.NoError(t, err)
					assert.Equal(t, int64(1), collection.FileCount)
				}
			})
		}
	}
}

func TestReplaceContentForIngestPublishesContentAndMembershipAtomically(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	firstRun, err := s.BeginIngest(ctx, "cli", "First synthetic import")
	require.NoError(t, err)
	first, _, err := s.IngestFile(ctx, firstRun, s.RootID(), "note.txt", fakeHash("a1"),
		4, "text/plain", "/synthetic/note.txt", "")
	require.NoError(t, err)

	secondRun, err := s.BeginIngest(ctx, "cli", "Replacement import")
	require.NoError(t, err)
	receipt, changed, err := s.ReplaceContentForIngest(
		ctx, secondRun, first.ID, first.Revision, fakeHash("b2"), 8,
		"text/markdown", "/synthetic/note.txt", "",
	)
	require.NoError(t, err)
	require.True(t, changed)
	assert.Equal(t, fakeHash("b2"), receipt.Node.BlobHash)
	assert.Equal(t, receipt.Version.ID, receipt.Node.CurrentVersionID)
	assert.Equal(t, first.Revision+2, receipt.Node.Revision)

	thirdRun, err := s.BeginIngest(ctx, "cli", "Unchanged import")
	require.NoError(t, err)
	confirmed, changed, err := s.ReplaceContentForIngest(
		ctx, thirdRun, first.ID, receipt.Node.Revision, fakeHash("b2"), 8,
		"text/markdown", "/synthetic/note.txt", "",
	)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, receipt.Version.ID, confirmed.Version.ID)
	assert.Equal(t, receipt.Node.Revision+1, confirmed.Node.Revision)
}

func TestReplaceContentForIngestRollsBackOnLabelConflict(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	label := "Synthetic batch"
	firstRun, err := s.BeginIngestWithLabel(ctx, "cli", "First", &label)
	require.NoError(t, err)
	first, _, err := s.IngestFileWithMembership(
		ctx, firstRun, s.RootID(), "note.txt", fakeHash("a1"), 4,
		"text/plain", "/synthetic/note.txt", "",
	)
	require.NoError(t, err)
	conflictRun, err := s.BeginIngestWithLabel(ctx, "cli", "Conflict", &label)
	require.NoError(t, err)

	_, _, err = s.ReplaceContentForIngest(
		ctx, conflictRun, first.ID, first.Revision, fakeHash("b2"), 8,
		"text/markdown", "/synthetic/note.txt", "",
	)
	require.ErrorIs(t, err, ErrExists)
	unchanged, err := s.NodeByID(ctx, first.ID)
	require.NoError(t, err)
	assert.Equal(t, first, unchanged)
	var versions, ingests int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM content_versions WHERE node_id=?),
		(SELECT COUNT(*) FROM ingests WHERE id=?)`, first.ID, conflictRun.ID()).Scan(&versions, &ingests))
	assert.Equal(t, int64(1), versions)
	assert.Zero(t, ingests)
}

func TestReplaceContentForIngestRejectsStaleRevisionBeforePublishingRun(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	firstRun, err := s.BeginIngest(ctx, "cli", "First")
	require.NoError(t, err)
	first, _, err := s.IngestFile(
		ctx, firstRun, s.RootID(), "note.txt", fakeHash("a1"), 4,
		"text/plain", "/synthetic/note.txt", "",
	)
	require.NoError(t, err)
	run, err := s.BeginIngest(ctx, "cli", "Stale")
	require.NoError(t, err)

	_, _, err = s.ReplaceContentForIngest(
		ctx, run, first.ID, first.Revision-1, fakeHash("b2"), 8,
		"text/markdown", "/synthetic/note.txt", "",
	)
	require.ErrorIs(t, err, ErrStaleRevision)
	unchanged, err := s.NodeByID(ctx, first.ID)
	require.NoError(t, err)
	assert.Equal(t, first, unchanged)
	var ingests, facts int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM ingests WHERE id=?),
		(SELECT COUNT(*) FROM provenance WHERE ingest_id=?)`, run.ID(), run.ID()).Scan(&ingests, &facts))
	assert.Zero(t, ingests)
	assert.Zero(t, facts)
}
