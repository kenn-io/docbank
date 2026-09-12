package store

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"

	"go.kenn.io/docbank/document/bundle"
)

// ExportMailboxCollectionMembers resolves the completed import's exact receipts
// in one read snapshot. Later heads and collection edits cannot replace them.
func (s *Store) ExportMailboxCollectionMembers(ctx context.Context, owner, collection string) ([]bundle.Member, error) {
	if owner == "" || validateUUIDv4(collection) != nil {
		return nil, bundle.ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	var count int
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(min(id),''),count(*) FROM (SELECT id FROM mailbox_jobs WHERE owner=? AND json_extract(job_json,'$.collection_id')=? ORDER BY id LIMIT 2)`, owner, collection).Scan(&id, &count)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, ErrNotFound
	}
	if count != 1 {
		return nil, bundle.ErrConflict
	}
	j, err := loadMailboxJob(ctx, tx, owner, id)
	if err != nil {
		return nil, err
	}
	if j.State != "complete" || j.CollectionID != collection {
		return nil, bundle.ErrConflict
	}
	if j.Checkpoint > bundle.MaxMembers {
		return nil, bundle.ErrLimit
	}
	settings, err := j.Settings.Canonical()
	if err != nil {
		return nil, err
	}
	var members []bundle.Member
	var after int64
	for {
		rows, err := mailboxOccurrences(ctx, tx, j.ID, after, 100)
		if err != nil {
			return nil, err
		}
		for _, o := range rows {
			if o.Ordinal != after+1 || o.Target == nil || (o.Outcome != "imported" && o.Outcome != "retry") || len(members) >= bundle.MaxMembers {
				return nil, bundle.ErrConflict
			}
			r, err := loadMailboxTransferReceipt(ctx, tx, o.ReceiptID)
			if err != nil {
				return nil, err
			}
			if r.Target != *o.Target || r.Location == nil || !reflect.DeepEqual(*r.Location, o.Location) || r.Request.ArchiveID != "mailbox:"+collection || r.Request.Reference != fmt.Sprintf("%d:%d", o.Location.EntryIndex, o.Location.Sequence) || r.Request.Settings != settings || r.Request.DestinationID != j.Settings.DestinationID {
				return nil, bundle.ErrConflict
			}
			m := o.Target
			members = append(members, bundle.Member{NodeID: m.NodeID, VersionID: m.VersionID, SHA256: m.SHA256, Size: m.Size})
			after = o.Ordinal
		}
		if len(rows) < 100 {
			break
		}
	}
	if after != j.Checkpoint || int64(len(members)) != j.Imported+j.Retries {
		return nil, bundle.ErrConflict
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return members, nil
}
