package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
)

type metadataMailboxJob struct {
	Type string     `json:"type"`
	Job  MailboxJob `json:"job"`
}
type metadataMailboxOccurrence struct {
	Type       string            `json:"type"`
	Occurrence MailboxOccurrence `json:"occurrence"`
}

func insertMailboxOccurrence(ctx context.Context, tx *sql.Tx, o MailboxOccurrence) error {
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	if len(b) > 64<<10 {
		return ErrMailboxLimit
	}
	var receipt any
	if o.ReceiptID != "" {
		receipt = o.ReceiptID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO mailbox_occurrences(job_id,ordinal,receipt_id,occurrence_json) VALUES(?,?,?,?)`, o.JobID, o.Ordinal, receipt, b)
	return err
}
func exportMailboxJobs(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	after := ""
	for {
		var owner, id string
		err := q.QueryRowContext(ctx, `SELECT owner,id FROM mailbox_jobs WHERE id>? ORDER BY id LIMIT 1`, after).Scan(&owner, &id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		j, err := loadMailboxJob(ctx, q, owner, id)
		if err != nil {
			return err
		}
		c, err := loadMailboxContainer(ctx, q, owner, j.ContainerID)
		if err != nil {
			return err
		}
		if c.State != "sealed" || c.SHA256 != j.ContainerSHA256 {
			return ErrMailboxInvalid
		}
		if err = write(metadataMailboxJob{Type: "mailbox_job", Job: j}); err != nil {
			return err
		}
		var ordinal, imported, rejected, retries, pending, canceled int64
		for {
			page, err := mailboxOccurrences(ctx, q, id, ordinal, 100)
			if err != nil {
				return err
			}
			if len(page) == 0 {
				break
			}
			for _, o := range page {
				if o.Ordinal != ordinal+1 || o.Location.ContainerID != j.ContainerID || o.Location.Sequence < 1 || o.Location.Start < 0 || o.Location.End <= o.Location.Start || !mailboxHash(o.Location.RawSHA256) || !mailboxHash(o.Location.EntrySHA256) || len(o.Location.Separator) > 4096 || len(o.Location.Labels) > 100 {
					return ErrMailboxInvalid
				}
				switch o.Outcome {
				case "pending", "canceled":
					if o.Ordinal != j.Checkpoint+1 || o.ReceiptID != "" {
						return ErrMailboxInvalid
					}
					if o.Outcome == "pending" {
						pending++
					} else {
						canceled++
					}
				case "imported", "retry":
					r, err := loadMailboxTransferReceipt(ctx, q, o.ReceiptID)
					if err != nil {
						return err
					}
					settings, err := j.Settings.Canonical()
					if err != nil {
						return err
					}
					if o.Target == nil || *o.Target != r.Target || r.Location == nil || !reflect.DeepEqual(*r.Location, o.Location) || r.Request.ArchiveID != "mailbox:"+j.CollectionID || r.Request.Reference != fmt.Sprintf("%d:%d", o.Location.EntryIndex, o.Location.Sequence) || r.Request.Settings != settings || r.Request.DestinationID != j.Settings.DestinationID {
						return ErrMailboxInvalid
					}
					if o.Outcome == "imported" {
						imported++
					} else {
						retries++
					}
				case "rejected":
					if !mailboxText(o.Reason, 4096) || o.ReceiptID != "" {
						return ErrMailboxInvalid
					}
					rejected++
				default:
					return ErrMailboxInvalid
				}
				if err = write(metadataMailboxOccurrence{Type: "mailbox_occurrence", Occurrence: o}); err != nil {
					return err
				}
				ordinal = o.Ordinal
			}
		}
		if ordinal != j.Checkpoint+pending+canceled || imported != j.Imported || rejected != j.Rejected || retries != j.Retries || pending != j.Pending || canceled != j.Canceled {
			return ErrMailboxInvalid
		}
		after = id
	}
}
