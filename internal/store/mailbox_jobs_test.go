package store

import (
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMailboxJobClaimCheckpointCancellationAndContinuation(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	req := MailboxContainerRequest{ID: "source", Owner: "one", SHA256: fakeHash("aa"), Size: 3, Format: "mbox"}
	_, err := s.BeginMailboxContainer(ctx, req)
	require.NoError(t, err)
	require.NoError(t, s.RecordRenditionBlob(ctx, req.SHA256, 3, BlobPhysical{Encoding: "raw", StoredBytes: 3, PackEligible: true, Created: true}))
	require.NoError(t, s.PutMailboxChunk(ctx, "one", "source", MailboxChunk{Index: 0, SHA256: req.SHA256, Size: 3}))
	_, err = s.SealMailboxContainer(ctx, "one", "source", req.SHA256, 3)
	require.NoError(t, err)
	request := MailboxJobRequest{ID: "job", ContainerID: "source", ContainerSHA256: req.SHA256, Settings: MailboxSettings{Dialect: "mboxrd", DestinationID: s.RootID()}}
	job, err := s.BeginMailboxJob(ctx, "one", request)
	require.NoError(t, err)
	claimed, err := s.ClaimMailboxJob(ctx)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	occurrence := MailboxOccurrence{JobID: job.ID, Ordinal: 1, Outcome: "rejected", Reason: "synthetic malformed message", Location: MailboxLocation{ContainerID: "source", Entry: "source.mbox", EntrySHA256: req.SHA256, Sequence: 1, Start: 0, End: 3, Separator: "From synthetic", RawSHA256: req.SHA256, Labels: []string{}}}
	require.NoError(t, s.CommitMailboxOccurrence(ctx, claimed.ID, claimed.Claim, occurrence, nil))
	pending := occurrence
	pending.Ordinal = 2
	pending.Outcome = "pending"
	pending.Reason = ""
	require.NoError(t, s.StartMailboxOccurrence(ctx, claimed.ID, claimed.Claim, pending))
	got, err := s.MailboxJob(ctx, "one", job.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), got.Checkpoint)
	require.Equal(t, int64(1), got.Rejected)
	require.NoError(t, s.CancelMailboxJob(ctx, "one", job.ID))
	canceled, err := s.MailboxJob(ctx, "one", job.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), canceled.Canceled)
	occurrence.Ordinal = 2
	require.Error(t, s.CommitMailboxOccurrence(ctx, claimed.ID, claimed.Claim, occurrence, nil))
	request.Settings.Dialect = "mboxo"
	_, err = s.ResumeMailboxJob(ctx, "one", request, false)
	require.Error(t, err)
	request.Settings.Dialect = "mboxrd"
	_, err = s.ResumeMailboxJob(ctx, "one", request, false)
	require.NoError(t, err)
	require.NoError(t, s.ResetMailboxClaims(ctx))
	newClaim, err := s.ClaimMailboxJob(ctx)
	require.NoError(t, err)
	require.NotEqual(t, claimed.Claim, newClaim.Claim)
	require.Error(t, s.CommitMailboxOccurrence(ctx, claimed.ID, claimed.Claim, occurrence, nil))
	require.NoError(t, s.FinishMailboxJob(ctx, newClaim.ID, newClaim.Claim, "partial", "segment limit", false))
	_, err = s.ResumeMailboxJob(ctx, "one", request, false)
	require.Error(t, err)
	next, err := s.ResumeMailboxJob(ctx, "one", request, true)
	require.NoError(t, err)
	require.Equal(t, int64(1), next.SegmentStart)
	require.Equal(t, job.CollectionID, next.CollectionID)
}

func TestMailboxCheckpointFailureRollsBackMessageAndReceipt(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	f := newEmailFixture(t, s, "seed.eml")
	source, err := emailVersion(ctx, s.db, f.publication.ContentVersionID)
	require.NoError(t, err)
	c := MailboxContainerRequest{ID: "source", Owner: "one", SHA256: source.BlobHash, Size: source.Size, Format: "mbox"}
	_, err = s.BeginMailboxContainer(ctx, c)
	require.NoError(t, err)
	require.NoError(t, s.PutMailboxChunk(ctx, c.Owner, c.ID, MailboxChunk{Index: 0, SHA256: c.SHA256, Size: c.Size}))
	_, err = s.SealMailboxContainer(ctx, c.Owner, c.ID, c.SHA256, c.Size)
	require.NoError(t, err)
	_, err = s.BeginMailboxJob(ctx, c.Owner, MailboxJobRequest{ID: "job", ContainerID: c.ID, ContainerSHA256: c.SHA256, Settings: MailboxSettings{DestinationID: s.RootID()}})
	require.NoError(t, err)
	j, err := s.ClaimMailboxJob(ctx)
	require.NoError(t, err)
	require.NoError(t, s.RegisterMailboxArchive(ctx, MailboxArchive{ID: "mailbox:" + j.CollectionID, Owner: c.Owner, Description: "Synthetic"}))
	settings, err := j.Settings.Canonical()
	require.NoError(t, err)
	location := MailboxLocation{ContainerID: c.ID, Entry: "source.mbox", EntrySHA256: c.SHA256, Sequence: 1, End: c.Size, RawSHA256: c.SHA256, EMLSHA256: c.SHA256, EMLSize: c.Size, Separator: "From synthetic", Labels: []string{}}
	o := MailboxOccurrence{JobID: j.ID, Ordinal: 1, Location: location}
	require.NoError(t, s.StartMailboxOccurrence(ctx, j.ID, j.Claim, o))
	p := MailboxTransferPublication{Owner: c.Owner, Request: MailboxTransferRequest{ArchiveID: "mailbox:" + j.CollectionID, Reference: "0:1", SHA256: c.SHA256, Size: c.Size, Settings: settings, DestinationID: s.RootID(), Name: "message.eml"}, Email: f.publication, Location: &location}
	var before int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM nodes`).Scan(&before))
	_, err = s.db.Exec(`CREATE TRIGGER fail_mailbox_checkpoint BEFORE UPDATE ON mailbox_jobs BEGIN SELECT RAISE(ABORT,'synthetic checkpoint rollback'); END`)
	require.NoError(t, err)
	require.Error(t, s.CommitMailboxOccurrence(ctx, j.ID, j.Claim, o, &p))
	var after, receipts int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM nodes`).Scan(&after))
	require.Equal(t, before, after)
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM mailbox_transfer_receipts`).Scan(&receipts))
	require.Zero(t, receipts)
	unchanged, err := s.MailboxJob(ctx, c.Owner, j.ID)
	require.NoError(t, err)
	require.Zero(t, unchanged.Checkpoint)
	require.Equal(t, int64(1), unchanged.Pending)
	_, err = s.db.Exec(`DROP TRIGGER fail_mailbox_checkpoint`)
	require.NoError(t, err)
	require.NoError(t, s.CommitMailboxOccurrence(ctx, j.ID, j.Claim, o, &p))
	require.NoError(t, s.ValidateMetadata(ctx))
	var original []byte
	require.NoError(t, s.db.QueryRow(`SELECT occurrence_json FROM mailbox_occurrences WHERE job_id=?`, j.ID).Scan(&original))
	var changed MailboxOccurrence
	require.NoError(t, json.Unmarshal(original, &changed))
	changed.Location.RawSHA256 = fakeHash("ab")
	raw, err := json.Marshal(changed)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE mailbox_occurrences SET occurrence_json=? WHERE job_id=?`, raw, j.ID)
	require.NoError(t, err)
	require.Error(t, s.ValidateMetadata(ctx), "occurrence and transfer location must agree exactly")
	_, err = s.db.Exec(`UPDATE mailbox_occurrences SET occurrence_json=? WHERE job_id=?`, original, j.ID)
	require.NoError(t, err)
	var originalReceipt []byte
	require.NoError(t, s.db.QueryRow(`SELECT receipt_json FROM mailbox_transfer_receipts`).Scan(&originalReceipt))
	for _, field := range []string{"hash", "size"} {
		t.Run("emitted_"+field, func(t *testing.T) {
			var receipt MailboxTransferReceipt
			require.NoError(t, json.Unmarshal(originalReceipt, &receipt))
			require.NoError(t, json.Unmarshal(original, &changed))
			if field == "hash" {
				changed.Location.EMLSHA256 = fakeHash("ab")
			} else {
				changed.Location.EMLSize++
			}
			receipt.Location = &changed.Location
			rawOccurrence, marshalErr := json.Marshal(changed)
			require.NoError(t, marshalErr)
			rawReceipt, marshalErr := json.Marshal(receipt)
			require.NoError(t, marshalErr)
			_, updateErr := s.db.Exec(`UPDATE mailbox_occurrences SET occurrence_json=? WHERE job_id=?`, rawOccurrence, j.ID)
			require.NoError(t, updateErr)
			_, updateErr = s.db.Exec(`UPDATE mailbox_transfer_receipts SET receipt_json=?`, rawReceipt)
			require.NoError(t, updateErr)
			require.Error(t, s.ValidateMetadata(ctx), "matching locations must still identify the retained EML target")
		})
	}
	_, err = s.db.Exec(`UPDATE mailbox_occurrences SET occurrence_json=? WHERE job_id=?`, original, j.ID)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE mailbox_transfer_receipts SET receipt_json=?`, originalReceipt)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestMailboxSettingsRefuseChangedDecoderOnResume(t *testing.T) {
	settings, err := normalizeMailboxSettings(MailboxSettings{DestinationID: 1})
	require.NoError(t, err)
	require.NotEmpty(t, settings.Recipe)
	settings.Recipe = "different-decoder"
	err = settings.ValidateExecution()
	require.ErrorIs(t, err, ErrMailboxConflict)
}

func TestMailboxCompleteCannotHidePendingOccurrence(t *testing.T) {
	j := MailboxJob{ID: "job", ContainerID: "source", ContainerSHA256: fakeHash("aa"), Settings: MailboxSettings{DestinationID: 1}, Owner: "one", State: "complete", CollectionID: "11111111-1111-4111-8111-111111111111", StartedAt: "2026-09-12T00:00:00.000000000Z", Checkpoint: 1, Imported: 1, Pending: 1, ScannedTail: true}
	require.Error(t, validateMailboxJob(j))
}
