package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
)

// StartMailboxOccurrence records one fully scanned, unpublished occurrence.
// Its checkpoint advances only with the final message or rejection receipt.
func (s *Store) StartMailboxOccurrence(ctx context.Context, id, claim string, o MailboxOccurrence) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		j, err := claimedMailboxJob(ctx, tx, id, claim, s.postgresSchema != "")
		if err != nil {
			return err
		}
		if o.JobID != id || o.Ordinal != j.Checkpoint+1 || o.Location.ContainerID != j.ContainerID || j.Checkpoint-j.SegmentStart >= MailboxSegmentMessages {
			return ErrMailboxConflict
		}
		prior, err := mailboxOccurrences(ctx, tx, id, j.Checkpoint, 1, s.postgresSchema != "")
		if err != nil {
			return err
		}
		if len(prior) > 0 {
			p := prior[0]
			if p.Ordinal != o.Ordinal || p.Location.RawSHA256 != o.Location.RawSHA256 || p.Location.Start != o.Location.Start || p.Location.End != o.Location.End || p.Location.EntrySHA256 != o.Location.EntrySHA256 {
				return ErrMailboxConflict
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM mailbox_occurrences WHERE job_id=$1 AND ordinal=$2`, storedString(s.postgresSchema != "", id), o.Ordinal); err != nil {
				return err
			}
		}
		o.Outcome = mailboxOccurrencePending
		o.Reason = ""
		o.ReceiptID = ""
		if err = insertMailboxOccurrence(ctx, tx, o, s.postgresSchema != ""); err != nil {
			return err
		}
		j.Pending = 1
		j.Canceled = 0
		return saveMailboxJob(ctx, tx, j, false, s.postgresSchema != "")
	})
}
func setMailboxPendingOutcome(ctx context.Context, tx *sql.Tx, j *MailboxJob, outcome string, postgres bool) error {
	pending, err := mailboxOccurrences(ctx, tx, j.ID, j.Checkpoint, 1, postgres)
	if err != nil {
		return err
	}
	j.Pending = 0
	j.Canceled = 0
	if len(pending) == 0 {
		return nil
	}
	o := pending[0]
	if o.Ordinal != j.Checkpoint+1 || (o.Outcome != mailboxOccurrencePending && o.Outcome != "canceled") {
		return ErrMailboxInvalid
	}
	o.Outcome = outcome
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE mailbox_occurrences SET occurrence_json=$1 WHERE job_id=$2 AND ordinal=$3`, b, storedString(postgres, j.ID), o.Ordinal); err != nil {
		return err
	}
	if outcome == mailboxOccurrencePending {
		j.Pending = 1
	} else {
		j.Canceled = 1
	}
	return nil
}
