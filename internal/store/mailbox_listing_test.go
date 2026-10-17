package store

import (
	"database/sql"
	"strings"
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
		j, err := s.BeginMailboxJob(ctx, c.Owner, MailboxJobRequest{ID: item.id, ContainerID: c.ID, ContainerSHA256: c.SHA256, Settings: MailboxSettings{DestinationID: s.RootID()}}, "")
		require.NoError(t, err)
		j.StartedAt = item.started
		require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error { return saveMailboxJob(ctx, tx, j, false) }))
		if j.ID == "newer-b" {
			_, err = s.ClaimMailboxJob(ctx)
			require.NoError(t, err)
		} else {
			require.NoError(t, s.CancelMailboxJob(ctx, c.Owner, j.ID))
		}
	}
	other := c
	other.ID, other.Owner = "other-source", "another-owner"
	_, err = s.BeginMailboxContainer(ctx, other)
	require.NoError(t, err)
	require.NoError(t, s.PutMailboxChunk(ctx, other.Owner, other.ID, MailboxChunk{SHA256: other.SHA256, Size: other.Size}))
	_, err = s.SealMailboxContainer(ctx, other.Owner, other.ID, other.SHA256, other.Size)
	require.NoError(t, err)
	_, err = s.BeginMailboxJob(ctx, other.Owner, MailboxJobRequest{
		ID: "other-job", ContainerID: other.ID, ContainerSHA256: other.SHA256,
		Settings: MailboxSettings{DestinationID: s.RootID()},
	}, "")
	require.NoError(t, err)
	// Page order must not depend on SQLite's traversal of the selected-row join.
	s.db.SetMaxOpenConns(1)
	_, err = s.db.ExecContext(ctx, `PRAGMA reverse_unordered_selects=ON`)
	require.NoError(t, err)
	first, err := s.MailboxJobs(ctx, c.Owner, "", 1)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Equal(t, "newer-b", first[0].ID)
	require.Equal(t, "running", first[0].State)
	require.NotEmpty(t, first[0].Claim)
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
	all, err := s.MailboxJobs(ctx, c.Owner, "", 100)
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Equal(t, []string{"newer-b", "newer-a", "oldest"}, []string{all[0].ID, all[1].ID, all[2].ID})

	var original []byte
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT job_json FROM mailbox_jobs WHERE id='oldest'`).Scan(&original))
	for _, tc := range []struct {
		name, update string
		want         error
	}{
		{"identity", `json_set(job_json, '$.id', 'different-job')`, ErrMailboxInvalid},
		{"state", `json_set(job_json, '$.state', 'queued')`, ErrMailboxInvalid},
		{"checkpoint", `json_set(job_json, '$.checkpoint', -1)`, ErrMailboxInvalid},
		{"unknown_member", `json_set(job_json, '$.unknown_field', 1)`, nil},
		{"oversized", `job_json || ?`, ErrMailboxLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var args []any
			if tc.name == "oversized" {
				args = append(args, strings.Repeat(" ", 128<<10))
			}
			_, err = s.db.ExecContext(ctx, `UPDATE mailbox_jobs SET job_json=`+tc.update+` WHERE id='oldest'`, args...)
			require.NoError(t, err)
			t.Cleanup(func() {
				_, err := s.db.ExecContext(ctx, `UPDATE mailbox_jobs SET job_json=? WHERE id='oldest'`, original)
				require.NoError(t, err)
			})
			_, err = s.MailboxJob(ctx, c.Owner, "oldest")
			require.Error(t, err)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			}
			page, err := s.MailboxJobs(ctx, c.Owner, "", 100)
			require.Error(t, err)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			}
			require.Nil(t, page, "invalid retained jobs must fail the whole page")
		})
	}
}
