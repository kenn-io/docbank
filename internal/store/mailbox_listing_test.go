package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMailboxJobsNewestFirstWithStablePages(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	f := newEmailFixture(t, s, "source.eml")
	source, err := emailVersion(ctx, s.db, f.publication.ContentVersionID)
	require.NoError(t, err)
	c := MailboxContainerRequest{ID: "source", Owner: "synthetic-owner", SHA256: source.BlobHash, Size: source.Size, Format: "mbox"}
	_, err = s.BeginMailboxContainer(ctx, c)
	require.NoError(t, err)
	require.NoError(t, s.PutMailboxChunk(ctx, c.Owner, c.ID, MailboxChunk{Index: 0, SHA256: c.SHA256, Size: c.Size}))
	_, err = s.SealMailboxContainer(ctx, c.Owner, c.ID, c.SHA256, c.Size)
	require.NoError(t, err)
	for _, item := range []struct{ id, started string }{
		{"oldest", "2026-01-01T00:00:00.000000000Z"},
		{"newer-b", "2026-02-01T00:00:00.000000000Z"},
		{"newer-a", "2026-02-01T00:00:00.000000000Z"},
	} {
		j, err := s.BeginMailboxJob(ctx, c.Owner, MailboxJobRequest{ID: item.id, ContainerID: c.ID, ContainerSHA256: c.SHA256, Settings: MailboxSettings{DestinationID: s.RootID()}})
		require.NoError(t, err)
		j.StartedAt = item.started
		require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error { return saveMailboxJob(ctx, tx, j, false) }))
		require.NoError(t, s.CancelMailboxJob(ctx, c.Owner, j.ID))
	}
	first, err := s.MailboxJobs(ctx, c.Owner, "", 1)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Equal(t, "newer-b", first[0].ID)
	second, err := s.MailboxJobs(ctx, c.Owner, first[0].ID, 2)
	require.NoError(t, err)
	require.Len(t, second, 2)
	require.Equal(t, "newer-a", second[0].ID)
	require.Equal(t, "oldest", second[1].ID)
	last, err := s.MailboxJobs(ctx, c.Owner, second[1].ID, 2)
	require.NoError(t, err)
	require.Empty(t, last)
	_, err = s.MailboxJobs(ctx, "another-owner", first[0].ID, 1)
	require.ErrorIs(t, err, ErrNotFound)
}
